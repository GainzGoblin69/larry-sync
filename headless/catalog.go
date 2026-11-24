package headless

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"

	"grout/cache"
	"grout/catalog"
	"grout/cfw"
	"grout/files"
	"grout/imaging"
	"grout/romm"
	"grout/settings"
)

const (
	// PlatformsFileName lists the platforms the launcher shows.
	PlatformsFileName = "platforms.tsv"
	// GamesDirName holds one file of games per platform.
	GamesDirName = "games"

	// Covers are fitted to this box. It is half of the Brick's 1024x768
	// screen, which is the largest the launcher draws one beside the list.
	artMaxWidth  = 512
	artMaxHeight = 384

	// artWorkers bounds concurrent cover downloads. Kept low for the same
	// reason grout bounds its save fetches: the server is often a small box on
	// the same wifi.
	artWorkers = 4
)

// SyncOptions selects what a catalog sync does beyond the game list.
type SyncOptions struct {
	// SkipArt leaves cover art for a later run.
	SkipArt bool
}

// Sync refreshes the whole catalog from the server and rewrites the files the
// launcher reads. Platforms that have no rom folder yet are given the
// firmware's own, so every game the device could run is listed whether or not
// it has been downloaded.
func (s *Session) Sync(options SyncOptions) error {
	if err := s.Online(); err != nil {
		return err
	}
	cm, err := catalogManager()
	if err != nil {
		return err
	}

	s.Events.Stage("platforms", "")
	if err := catalog.LoadPlatformsBinding(s.Config, s.Host, s.Config.ApiTimeout.Duration()); err != nil {
		slog.Default().Debug("Failed to load platform bindings", "error", err)
	} else {
		s.State.PlatformsBinding = s.Config.PlatformsBinding
	}

	platforms, err := s.client().GetPlatforms()
	if err != nil {
		return fmt.Errorf("loading platforms: %w", err)
	}
	// Saved as the server sent them. Names are only made unique when they are
	// read back, which is also how grout stores them.
	if err := cm.SavePlatforms(platforms); err != nil {
		return fmt.Errorf("saving platforms: %w", err)
	}

	if err := s.mapNewPlatforms(platforms); err != nil {
		return err
	}
	// The catalog reads settings from the copy it was opened with, so it has
	// to be told about folders mapped a moment ago.
	if err := cache.InitCacheManager(s.Host, *s.Config); err != nil {
		return fmt.Errorf("reopening the catalog: %w", err)
	}

	mapped := s.mappedPlatforms(platforms)
	s.Events.Stage("games", strconv.Itoa(len(mapped)))

	progress := atomic.NewFloat64(0)
	stop := s.watch(progress)
	stats, err := cm.PopulateFullCacheWithProgress(mapped, progress)
	stop()
	if err != nil {
		return fmt.Errorf("loading games: %w", err)
	}

	if !options.SkipArt {
		s.syncArt(cm, mapped)
	}

	s.State.LastSync = time.Now().UTC()
	s.saveState()

	if err := s.Export(); err != nil {
		return err
	}

	s.Events.Result("platforms", strconv.Itoa(stats.Platforms))
	s.Events.Result("games_updated", strconv.Itoa(stats.GamesUpdated))
	return nil
}

// mapNewPlatforms gives every platform without a rom folder the one the
// firmware expects, and stores the result. Platforms the firmware has no
// folder for stay unmapped, which is what keeps them off the device.
func (s *Session) mapNewPlatforms(platforms []romm.Platform) error {
	var unmapped []romm.Platform
	for _, platform := range platforms {
		if _, ok := s.Config.DirectoryMappings[platform.FSSlug]; !ok && platform.ROMCount > 0 {
			unmapped = append(unmapped, platform)
		}
	}
	if len(unmapped) == 0 {
		return nil
	}

	romDirectory := cfw.GetRomDirectory()
	directories, err := files.SubdirectoryNames(romDirectory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading rom directory %s: %w", romDirectory, err)
	}

	// Existing is left empty on purpose: with it set, a platform that has no
	// stored mapping is treated as one the user chose to skip.
	detected := catalog.DetectMappings(unmapped, catalog.ChoiceRequest{
		CFW:              s.CFW,
		Directories:      directories,
		PlatformsBinding: s.Config.PlatformsBinding,
		AutoSelect:       true,
	})
	if len(detected) == 0 {
		return nil
	}

	if s.Config.DirectoryMappings == nil {
		s.Config.DirectoryMappings = make(map[string]settings.DirectoryMapping, len(detected))
	}
	for fsSlug, mapping := range detected {
		s.Config.DirectoryMappings[fsSlug] = mapping
	}
	if err := settings.SaveConfig(s.Config); err != nil {
		return fmt.Errorf("saving platform folders: %w", err)
	}

	s.Events.Info("Mapped %d new platform(s) to rom folders", len(detected))
	return nil
}

// mappedPlatforms keeps the platforms that have a rom folder and games, in the
// order the user arranged them.
func (s *Session) mappedPlatforms(platforms []romm.Platform) []romm.Platform {
	mapped := make([]romm.Platform, 0, len(platforms))
	for _, platform := range platforms {
		if _, ok := s.Config.DirectoryMappings[platform.FSSlug]; ok && platform.ROMCount > 0 {
			mapped = append(mapped, platform)
		}
	}
	return catalog.SortByOrder(mapped, s.Config.PlatformOrder)
}

// watch reports a 0..1 progress value as it moves, until the returned function
// is called.
func (s *Session) watch(progress *atomic.Float64) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.Events.Progress(int64(progress.Load()*100), 100)
			}
		}
	}()

	return func() {
		close(done)
		wg.Wait()
		s.Events.Progress(int64(progress.Load()*100), 100)
	}
}

// syncArt fetches the cover of every game that has one and is missing it.
//
// Covers go in grout's own artwork cache, keyed by rom id, so a game has a
// cover to show before it has ever been downloaded. A cover that fails is
// passed over: the next sync tries again, and a list with a gap in it is
// better than no list.
func (s *Session) syncArt(cm *cache.Manager, platforms []romm.Platform) {
	var missing []romm.Rom
	for _, platform := range platforms {
		games, err := cm.GetPlatformGames(platform.ID)
		if err != nil {
			slog.Default().Warn("Failed to read games for artwork", "platform", platform.Name, "error", err)
			continue
		}
		missing = append(missing, cache.GetMissingArtwork(games)...)
	}

	s.Events.Stage("art", strconv.Itoa(len(missing)))
	if len(missing) == 0 {
		return
	}

	// One fetcher for the run, so connections are reused across what can be
	// thousands of covers. The box is passed in because the usual resizer asks
	// the window for its size, and there is no window here.
	fetcher := romm.NewArtFetcher(s.Host, romm.DefaultClientTimeout)
	fetcher.Process = func(path string) error {
		return imaging.ProcessArtImageTo(path, artMaxWidth, artMaxHeight)
	}

	var (
		done   atomic.Int64
		failed atomic.Int64
		wg     sync.WaitGroup
	)
	queue := make(chan romm.Rom)
	total := int64(len(missing))

	for i := 0; i < artWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for game := range queue {
				if err := s.fetchCover(fetcher, game); err != nil {
					failed.Inc()
					slog.Default().Debug("Failed to download cover", "game", game.Name, "error", err)
				}
				s.Events.Progress(done.Inc(), total)
			}
		}()
	}

	for _, game := range missing {
		queue <- game
	}
	close(queue)
	wg.Wait()

	if count := failed.Load(); count > 0 {
		s.Events.Info("%d cover(s) could not be downloaded", count)
	}
}

func (s *Session) fetchCover(fetcher *romm.ArtFetcher, game romm.Rom) error {
	source := game.GetArtworkURL(s.Config.ArtKind, s.Host)
	if source == "" {
		return nil
	}
	if err := cache.EnsureArtworkCacheDir(game.PlatformFSSlug); err != nil {
		return err
	}
	return fetcher.Save(source, cache.GetArtworkCachePath(game.PlatformFSSlug, game.ID))
}

// Export rewrites the files the launcher reads from the local catalog. It
// needs no network, so the launcher can call it on every boot.
//
// platforms.tsv has one platform per line:
//
//	fs_slug  name  rom_directory  games  downloaded
//
// games/<fs_slug>.tsv has one game per line, sorted by name:
//
//	rom_id  name  downloaded  local_path  art_path  size_bytes  file_name
//
// downloaded is 1 or 0. local_path and art_path are empty when there is no
// file. Fields are separated by tabs and never contain one.
func (s *Session) Export() error {
	cm, err := catalogManager()
	if err != nil {
		return err
	}

	platforms, err := cm.GetPlatforms()
	if err != nil {
		return fmt.Errorf("reading platforms: %w", err)
	}
	romm.DisambiguatePlatformNames(platforms)
	platforms = s.mappedPlatforms(platforms)

	gamesDir := filepath.Join(s.ExportDir, GamesDirName)
	if err := os.MkdirAll(gamesDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", gamesDir, err)
	}

	var index strings.Builder
	written := make(map[string]bool, len(platforms))
	totalGames, totalDownloaded := 0, 0

	for _, platform := range platforms {
		games, err := cm.GetPlatformGames(platform.ID)
		if err != nil {
			return fmt.Errorf("reading games for %s: %w", platform.Name, err)
		}
		if len(games) == 0 {
			continue
		}

		fileName := exportFileName(platform.FSSlug)
		downloaded, err := s.exportPlatform(filepath.Join(gamesDir, fileName), platform, games)
		if err != nil {
			return err
		}
		written[fileName] = true
		totalGames += len(games)
		totalDownloaded += downloaded

		index.WriteString(strings.Join([]string{
			cleanField(platform.FSSlug),
			cleanField(platform.DisplayName()),
			cleanField(cfw.PlatformRomDirectory(*s.Config, platform.FSSlug)),
			strconv.Itoa(len(games)),
			strconv.Itoa(downloaded),
		}, "\t"))
		index.WriteByte('\n')
	}

	if err := files.WriteFileAtomic(filepath.Join(s.ExportDir, PlatformsFileName), []byte(index.String()), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", PlatformsFileName, err)
	}

	// A platform dropped from the server would otherwise leave its list behind
	// for the launcher to keep showing.
	removeStale(gamesDir, written)

	s.Events.Result("export_dir", s.ExportDir)
	s.Events.Result("games", strconv.Itoa(totalGames))
	s.Events.Result("downloaded", strconv.Itoa(totalDownloaded))
	return nil
}

// exportPlatform writes one platform's games and returns how many are on the
// device.
func (s *Session) exportPlatform(path string, platform romm.Platform, games []romm.Rom) (int, error) {
	sort.SliceStable(games, func(i, j int) bool {
		return strings.ToLower(displayName(games[i])) < strings.ToLower(displayName(games[j]))
	})

	// Asking whether each game is present costs a look at the card per file.
	// A platform whose folder does not exist has nothing downloaded, which is
	// most platforms on a device that fetches on demand.
	romDirectory := cfw.PlatformRomDirectory(*s.Config, platform.FSSlug)
	folderExists := files.FileExists(romDirectory)

	var out strings.Builder
	downloaded := 0

	for _, game := range games {
		localPath := ""
		if folderExists {
			localPath = s.localPath(game)
		}
		present := "0"
		if localPath != "" {
			present = "1"
			downloaded++
		}

		artPath := ""
		if cache.ArtworkExists(game.PlatformFSSlug, game.ID) {
			artPath = cache.GetArtworkCachePath(game.PlatformFSSlug, game.ID)
		}

		out.WriteString(strings.Join([]string{
			strconv.Itoa(game.ID),
			cleanField(displayName(game)),
			present,
			cleanField(localPath),
			cleanField(artPath),
			strconv.FormatInt(game.FsSizeBytes, 10),
			cleanField(game.EntryFileName()),
		}, "\t"))
		out.WriteByte('\n')
	}

	if err := files.WriteFileAtomic(path, []byte(out.String()), 0644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", path, err)
	}
	return downloaded, nil
}

// localPath is the file that holds a game on the device, or "". The catalog's
// own answer comes first; the recorded path covers a game that was unpacked to
// a name the server never listed.
func (s *Session) localPath(game romm.Rom) string {
	if path := catalog.LocalRomPath(*s.Config, game); path != "" {
		return path
	}
	return s.State.DownloadedPath(game.ID)
}

// displayName is what a game is called in a list. Unidentified games have no
// name on the server, only the file they were found as.
func displayName(game romm.Rom) string {
	if name := strings.TrimSpace(game.Name); name != "" {
		return name
	}
	if game.FsNameNoExt != "" {
		return game.FsNameNoExt
	}
	return game.EntryFileName()
}

// exportFileName turns a platform slug into a file name. Slugs are plain in
// practice, but they come from the server and name a file here.
func exportFileName(fsSlug string) string {
	safe := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '\t', '\n', '\r':
			return '_'
		}
		return r
	}, fsSlug)
	safe = strings.Trim(safe, ".")
	if safe == "" {
		safe = "_"
	}
	return safe + ".tsv"
}

func removeStale(dir string, keep map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || keep[entry.Name()] || !strings.HasSuffix(entry.Name(), ".tsv") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			slog.Default().Debug("Failed to remove stale catalog file", "file", entry.Name(), "error", err)
		}
	}
}
