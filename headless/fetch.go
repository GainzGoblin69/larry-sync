package headless

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/atomic"

	"grout/catalog"
	"grout/cfw"
	"grout/download"
	"grout/files"
	"grout/imaging"
	"grout/romm"
)

// FetchOptions selects what fetching a game brings with it.
type FetchOptions struct {
	// RomID is the game to fetch.
	RomID int
	// FileID picks one version of a game that ships several. Zero takes the
	// first a player would launch.
	FileID int
	// SkipBIOS leaves the platform's BIOS files alone.
	SkipBIOS bool
	// SkipSaves leaves the game's saves alone.
	SkipSaves bool
}

// Fetch makes a game ready to play: the rom itself, any BIOS its platform
// needs, and the newest save the server holds for it.
//
// Only the rom is required. A missing BIOS or save is reported and passed
// over, since most games run without either and the launcher can still start
// the one that was asked for. The path to launch is reported as RESULT
// rom_path.
func (s *Session) Fetch(ctx context.Context, options FetchOptions) error {
	game, err := s.game(options.RomID)
	if err != nil {
		return err
	}
	platform := romm.Platform{ID: game.PlatformID, FSSlug: game.PlatformFSSlug, Name: game.PlatformDisplayName}

	// Asked once: every later step would otherwise wait out its own timeout
	// on a device that is simply away from home.
	offline := s.Online()

	path := s.localPath(game)
	if path == "" {
		if offline != nil {
			return offline
		}
		path, err = s.fetchRom(ctx, game, platform, options.FileID)
		if err != nil {
			return err
		}
	} else {
		s.Events.Info("%s is already on the device", displayName(game))

		// Downloaded before archives were unpacked here, and still zipped: the
		// emulator cannot open it, so it is unpacked now.
		if !game.HasMultipleFiles && download.IsArchive(path) && s.unpacks(platform) {
			unpacked, err := s.unpack(game, platform, path)
			if err != nil {
				return err
			}
			if unpacked != path {
				s.recordDownload(game, unpacked)
				path = unpacked
			}
		}
	}

	if offline != nil {
		s.Events.Info("Server not reachable, skipping the BIOS and save checks")
	} else {
		if !options.SkipBIOS {
			if _, err := s.EnsureBIOS(ctx, platform); err != nil {
				s.Events.Info("BIOS files could not be checked: %v", err)
			}
		}
		if !options.SkipSaves {
			if _, err := s.syncSaves(SaveOptions{RomID: game.ID, Direction: DirectionPull}); err != nil {
				s.Events.Info("Saves could not be checked: %v", err)
			}
		}
	}

	// Only the one platform changed, but the index carries per-platform
	// counts, and rewriting everything is quick next to a download.
	if err := s.Export(); err != nil {
		slog.Default().Warn("Failed to rewrite the catalog files", "error", err)
	}

	s.Events.Result("rom_path", path)
	return nil
}

// game reads one game from the local catalog.
func (s *Session) game(romID int) (romm.Rom, error) {
	cm, err := catalogManager()
	if err != nil {
		return romm.Rom{}, err
	}
	games, err := cm.GetGamesByIDs([]int{romID})
	if err != nil {
		return romm.Rom{}, fmt.Errorf("reading the catalog: %w", err)
	}
	if len(games) == 0 {
		return romm.Rom{}, fmt.Errorf("%w: rom %d", ErrNotInCatalog, romID)
	}
	return games[0], nil
}

// fetchRom downloads a game's file and unpacks it if it needs unpacking, and
// returns the path to launch.
func (s *Session) fetchRom(ctx context.Context, game romm.Rom, platform romm.Platform, fileID int) (string, error) {
	plan, skipped := download.BuildPlan(*s.Config, s.Host, platform, []romm.Rom{game}, fileID)
	if len(plan.Roms) == 0 {
		if len(skipped) > 0 {
			return "", fmt.Errorf("%s cannot be downloaded: %s", displayName(game), skipped[0].Reason)
		}
		return "", fmt.Errorf("%s cannot be downloaded", displayName(game))
	}
	item := plan.Roms[0]

	// The file is named by the server. A multi-file game goes to a fixed
	// temporary name; anything else has to land in its own rom folder.
	if romDirectory := cfw.PlatformRomDirectory(*s.Config, platform.FSSlug); !game.HasMultipleFiles && !isInside(romDirectory, item.Location) {
		return "", fmt.Errorf("refusing to write %s: it is outside %s", item.Location, romDirectory)
	}

	s.Events.Stage("rom", displayName(game))
	fetcher := newTransfer(s.Host, s.Config.DownloadTimeout.Duration())
	if err := fetcher.fetch(ctx, item.URL, item.Location, game.FsSizeBytes, s.Events.Progress); err != nil {
		return "", err
	}

	path, err := s.unpack(game, platform, item.Location)
	if err != nil {
		return "", err
	}
	plan.SetGamePath(game.EntryFileName(), path)

	if len(plan.Art) > 0 {
		s.Events.Stage("art", displayName(game))
		artFetcher := romm.NewArtFetcher(s.Host, romm.DefaultClientTimeout)
		artFetcher.Process = func(path string) error {
			return imaging.ProcessArtImageTo(path, artMaxWidth, artMaxHeight)
		}
		download.FetchArt(artFetcher, plan.Art, []romm.Rom{game}, nil)
	}

	cfw.FillGamesMetadata(plan.Entries)

	s.recordDownload(game, path)
	return path, nil
}

// recordDownload notes where a game was put, so later commands find it.
func (s *Session) recordDownload(game romm.Rom, path string) {
	size := game.FsSizeBytes
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		size = info.Size()
	}
	s.State.RecordDownload(game.ID, path, size)
	s.saveState()
}

// archiveTags are the tags of rom folders whose emulators load a game's zip
// as it is: arcade cores, which want each game's zip intact.
var archiveTags = map[string]bool{
	"FBN": true, "FBNEO": true, "MAME": true, "ARCADE": true,
	"NEOGEO": true, "CPS1": true, "CPS2": true, "CPS3": true,
}

// keepsArchives reports whether games in romDirectory stay zipped, going by
// the tag at the end of the folder's name, as in "Arcade (FBN)".
func keepsArchives(romDirectory string) bool {
	base := filepath.Base(romDirectory)
	start := strings.LastIndex(base, "(")
	end := strings.LastIndex(base, ")")
	if start < 0 || end < start {
		return false
	}
	return archiveTags[strings.ToUpper(strings.TrimSpace(base[start+1:end]))]
}

// unpacks reports whether a game for platform that arrives as a single
// archive is unpacked. NextUI's emulator opens no archives itself, so every
// system but arcade needs its games unpacked. Grout's own setting, for anyone
// who turned it on, unpacks arcade games too.
func (s *Session) unpacks(platform romm.Platform) bool {
	if s.Config.UnzipDownloads {
		return true
	}
	return !keepsArchives(cfw.PlatformRomDirectory(*s.Config, platform.FSSlug))
}

// unpack expands a game that arrived as an archive and returns the path to
// launch. A game that needs no unpacking comes back as it landed.
func (s *Session) unpack(game romm.Rom, platform romm.Platform, location string) (string, error) {
	switch {
	case game.HasMultipleFiles:
		// A game shipped as several files always arrives zipped, and is not
		// playable until it is laid out the way the firmware expects.
		s.Events.Stage("extract", displayName(game))
		progress := atomic.NewFloat64(0)
		stop := s.watch(progress)
		path, err := download.ExtractMultiFile(game, cfw.PlatformRomDirectory(*s.Config, platform.FSSlug), progress)
		stop()
		if err != nil {
			return "", err
		}
		return path, nil

	case download.IsArchive(location) && s.unpacks(platform):
		s.Events.Stage("extract", displayName(game))
		progress := atomic.NewFloat64(0)
		stop := s.watch(progress)
		path, err := download.ExtractArchive(location, filepath.Dir(location), progress)
		stop()
		if err != nil {
			// The archive is still there, which beats losing the download;
			// the error says why the game may not start.
			s.Events.Info("Could not unpack %s, keeping the archive: %v", filepath.Base(location), err)
			return location, nil
		}
		return path, nil
	}

	return location, nil
}

// Remove deletes a downloaded game from the device, leaving it in the catalog
// to be fetched again. Saves are untouched.
func (s *Session) Remove(romID int) error {
	game, err := s.game(romID)
	if err != nil {
		return err
	}

	path := s.localPath(game)
	if path == "" {
		return fmt.Errorf("%s is not on the device", displayName(game))
	}

	// Names come from the server, and one holding ".." would otherwise point
	// a delete at something that is not this game.
	romDirectory := catalog.RomDirectory(*s.Config, game)
	if !isInside(romDirectory, path) {
		return fmt.Errorf("refusing to remove %s: it is outside %s", path, romDirectory)
	}

	// A game laid out as a folder of discs is launched through a playlist
	// beside or inside that folder, so the folder goes with it.
	if game.HasMultipleFiles {
		folder := filepath.Join(romDirectory, game.FsNameNoExt)
		if isInside(romDirectory, folder) && files.FileExists(folder) {
			if err := os.RemoveAll(folder); err != nil {
				return fmt.Errorf("removing %s: %w", folder, err)
			}
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	delete(s.State.Downloads, strconv.Itoa(romID))
	s.saveState()

	if err := s.Export(); err != nil {
		slog.Default().Warn("Failed to rewrite the catalog files", "error", err)
	}

	s.Events.Result("removed", path)
	return nil
}

// isInside reports whether child is strictly below parent.
func isInside(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
