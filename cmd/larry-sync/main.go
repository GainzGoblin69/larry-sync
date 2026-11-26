// larry-sync is grout without a screen: the RomM catalog, downloads, BIOS files
// and save sync as commands a launcher runs and reads the output of.
//
// Run it from the folder that holds grout's config.json, with the same CFW
// environment variable grout's launch script sets. It shares grout's settings
// and catalog, so a device paired in grout is paired here and the reverse.
//
// Standard output carries only the line protocol described in package
// headless. Logs go to standard error and to logs/larry-sync.log.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	_ "github.com/BrandonKowalski/certifiable"
	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"

	"grout/cfw"
	"grout/headless"
	"grout/settings"
	"grout/version"
)

// Exit codes, so a launcher can branch without reading the error text.
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitNotPaired   = 3
	exitUnreachable = 4
)

const usage = `larry-sync: the RomM catalog, downloads and save sync without a screen.

Usage: larry-sync <command> [options]

Commands:
  pair --url URL [--name NAME] [--insecure]
        Link this device to a RomM server. Prints the address to approve it at.
  status
        Report whether the device is paired and the server answers.
  sync [--no-art]
        Refresh the whole catalog and cover art, then write the catalog files.
  export
        Rewrite the catalog files from the local catalog. Needs no network.
  fetch --rom ID [--file ID] [--no-bios] [--no-saves]
        Download a game, the BIOS files its platform needs and its newest save.
  remove --rom ID
        Delete a downloaded game from the device. Saves are kept.
  bios --platform FS_SLUG
        Download any BIOS files missing for a platform.
  saves [--rom ID] [--direction both|pull|push]
        Sync saves with the server. Conflicts are reported, never overwritten.
  list-saves (--rom ID | --path FILE)
        List a game's saves on the server, newest first, one SAVE line each.
  choose-save (--rom ID | --path FILE) --save ID
        Put one of a game's server saves on the device in place of its own,
        which is backed up first, and keep it chosen for later syncs.
        Each save's picture follows the list as a SHOT line.
  update check|download [--card PATH]
        Check the firmware's GitHub releases for a newer build, or download
        its MinUI.zip to the top of the card for the boot script to install.
  music next [--source game|johnny] [--card PATH]
        Bring down the next song for the menu music, from the Copyparty
        folders set in music.txt, and print where it is.
  version
        Print the version.

Output is one event per line, fields separated by tabs:
  STAGE <name> <detail> | PROGRESS <percent> <done> <total> | INFO <message>
  RESULT <key> <value>  | ERROR <message>
  SAVE <id> <slot> <updated> <age> <bytes> <newest in slot> <on this device>
       <devices> <emulator> <file> <has a picture>
  SHOT <save id> <path of its picture>

Exit codes: 0 ok, 1 failed, 2 bad usage, 3 not paired, 4 server unreachable.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// The toolkit's logger copies everything to whatever os.Stdout is when it
	// first starts. Standard output is the protocol here, so the real one is
	// kept aside and the logger is given standard error to copy to instead.
	protocol := os.Stdout
	os.Stdout = os.Stderr

	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	}
	command, rest := args[0], args[1:]

	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(protocol, usage)
		return exitOK
	case "version":
		fmt.Fprintln(protocol, version.Get().Version)
		return exitOK
	}

	gaba.SetLogFilename("larry-sync.log")
	slog.SetDefault(gaba.GetLogger())
	gaba.SetLogLevel(logLevel())

	events := headless.NewEvents(protocol)

	// Powering off mid-download is routine on a handheld. Cancelling the
	// context lets a transfer remove its partial file before the process ends.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := dispatch(ctx, events, command, rest)
	if err == nil {
		return exitOK
	}

	var usageErr usageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(os.Stderr, "larry-sync %s: %v\n\n%s", command, err, usage)
		return exitUsage
	}

	slog.Default().Error("Command failed", "command", command, "error", err)
	events.Error(err)

	switch {
	case errors.Is(err, headless.ErrNotPaired):
		return exitNotPaired
	case errors.Is(err, headless.ErrUnreachable):
		return exitUnreachable
	}
	return exitFailed
}

// usageError is a mistake in how the command was typed, as opposed to the
// command failing.
type usageError struct{ error }

func dispatch(ctx context.Context, events *headless.Events, command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	switch command {
	case "pair":
		url := flags.String("url", "", "server address")
		name := flags.String("name", "", "device name shown in RomM")
		insecure := flags.Bool("insecure", false, "accept a self-signed certificate")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *url == "" {
			return usageError{errors.New("--url is required")}
		}
		return headless.Pair(ctx, events, headless.PairOptions{
			URL:                *url,
			DeviceName:         *name,
			InsecureSkipVerify: *insecure,
		})

	case "status":
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		return status(events)

	case "sync":
		noArt := flags.Bool("no-art", false, "leave cover art for a later run")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.Sync(headless.SyncOptions{SkipArt: *noArt})
		})

	case "export":
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.Export()
		})

	case "fetch":
		rom := flags.Int("rom", 0, "rom id")
		file := flags.Int("file", 0, "file id, for a game with several versions")
		noBIOS := flags.Bool("no-bios", false, "leave BIOS files alone")
		noSaves := flags.Bool("no-saves", false, "leave saves alone")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *rom <= 0 {
			return usageError{errors.New("--rom is required")}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.Fetch(ctx, headless.FetchOptions{
				RomID:     *rom,
				FileID:    *file,
				SkipBIOS:  *noBIOS,
				SkipSaves: *noSaves,
			})
		})

	case "remove":
		rom := flags.Int("rom", 0, "rom id")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *rom <= 0 {
			return usageError{errors.New("--rom is required")}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.Remove(*rom)
		})

	case "bios":
		slug := flags.String("platform", "", "platform fs_slug, as listed in platforms.tsv")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *slug == "" {
			return usageError{errors.New("--platform is required")}
		}
		return withSession(events, func(session *headless.Session) error {
			if err := session.Online(); err != nil {
				return err
			}
			platform, err := session.PlatformBySlug(*slug)
			if err != nil {
				return err
			}
			_, err = session.EnsureBIOS(ctx, platform)
			return err
		})

	case "saves":
		rom := flags.Int("rom", 0, "limit to one rom id")
		direction := flags.String("direction", string(headless.DirectionBoth), "both, pull or push")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		parsed, err := headless.ParseDirection(*direction)
		if err != nil {
			return usageError{err}
		}
		return withSession(events, func(session *headless.Session) error {
			_, err := session.SyncSaves(headless.SaveOptions{RomID: *rom, Direction: parsed})
			return err
		})

	case "update":
		card := flags.String("card", cardPath(), "the top of the SD card")
		if len(args) == 0 {
			return usageError{errors.New("give check or download")}
		}
		action, rest := args[0], args[1:]
		if err := flags.Parse(rest); err != nil {
			return usageError{err}
		}
		updater := headless.NewUpdater(*card, events)
		switch action {
		case "check":
			_, _, err := updater.Check(ctx)
			return err
		case "download":
			return updater.Download(ctx)
		}
		return usageError{fmt.Errorf("unknown update action %q: give check or download", action)}

	case "music":
		source := flags.String("source", headless.MusicGame, "game or johnny")
		card := flags.String("card", cardPath(), "the top of the SD card")
		if len(args) == 0 || args[0] != "next" {
			return usageError{errors.New("give next")}
		}
		if err := flags.Parse(args[1:]); err != nil {
			return usageError{err}
		}
		if *source != headless.MusicGame && *source != headless.MusicJohnny {
			return usageError{fmt.Errorf("unknown music %q: give game or johnny", *source)}
		}
		// beside the catalog, as the save pictures are
		dir := filepath.Join(filepath.Dir(headless.ExportDir()), "music")
		return headless.NewMusic(*card, dir, events).Next(ctx, *source)

	case "list-saves":
		rom := flags.Int("rom", 0, "rom id")
		path := flags.String("path", "", "the game's file on the device")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *rom == 0 && *path == "" {
			return usageError{headless.ErrNoGame}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.ListSaves(headless.GameRef{RomID: *rom, Path: *path})
		})

	case "choose-save":
		rom := flags.Int("rom", 0, "rom id")
		path := flags.String("path", "", "the game's file on the device")
		save := flags.Int("save", 0, "save id, from list-saves")
		if err := flags.Parse(args); err != nil {
			return usageError{err}
		}
		if *rom == 0 && *path == "" {
			return usageError{headless.ErrNoGame}
		}
		if *save == 0 {
			return usageError{errors.New("give --save")}
		}
		return withSession(events, func(session *headless.Session) error {
			return session.ChooseSave(headless.GameRef{RomID: *rom, Path: *path}, *save)
		})
	}

	return usageError{fmt.Errorf("unknown command %q", command)}
}

// cardPath is the top of the SD card: SDCARD_PATH as NextUI sets it, or where
// the Brick mounts it.
func cardPath() string {
	if path := os.Getenv("SDCARD_PATH"); path != "" {
		return path
	}
	return "/mnt/SDCARD"
}

// withSession opens the settings and catalog for one command and closes them
// after it.
func withSession(events *headless.Events, work func(*headless.Session) error) error {
	session, err := headless.Open(events)
	if err != nil {
		return err
	}
	defer session.Close()
	return work(session)
}

// status reports pairing and reachability as results rather than failing, so a
// launcher can ask once and decide what to show.
func status(events *headless.Events) error {
	active, err := cfw.Active()
	if err != nil {
		return err
	}
	events.Result("cfw", string(active))
	events.Result("version", version.Get().Version)

	session, err := headless.Open(events)
	if errors.Is(err, headless.ErrNotPaired) {
		events.Result("paired", "0")
		events.Result("online", "0")
		return nil
	}
	if err != nil {
		return err
	}
	defer session.Close()

	events.Result("server", session.Host.URL())
	events.Result("username", session.Host.Username)
	events.Result("save_sync", strconv.FormatBool(session.Host.DeviceID != ""))
	if !session.State.LastSync.IsZero() {
		events.Result("last_sync", session.State.LastSync.Format("2006-01-02T15:04:05Z"))
	}

	switch err := session.Online(); {
	case err == nil:
		events.Result("paired", "1")
		events.Result("online", "1")
	case errors.Is(err, headless.ErrNotPaired):
		events.Result("paired", "0")
		events.Result("online", "1")
	default:
		events.Result("paired", "1")
		events.Result("online", "0")
	}
	return nil
}

// logLevel is info, or debug when the stored settings ask for it. Grout's
// own default is errors only, which left nothing in the log on the card to
// tell why a save did not come down; the few info lines larry-sync writes per
// command are what make that traceable.
func logLevel() slog.Level {
	config, err := settings.LoadConfig()
	if err == nil && config.LogLevel == settings.LogLevelDebug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
