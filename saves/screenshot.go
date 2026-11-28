package saves

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"grout/settings"
)

// screenshotFresh is how far apart a save and a picture may have been
// written and still be the same moment in the game. The emulator writes both
// when a game is left.
const screenshotFresh = 10 * time.Minute

// screenshotFor is the picture the device took when the save at savePath was
// last written, or "" when there is none that recent.
//
// Pictures are kept at <ScreenshotDir>/<emulator folder>/<game file>.png. A
// save is named after the game file plus its own extension, as
// "Game.sfc.sav", or after the game's name alone, as "Game.srm", so either
// "Game.sfc.png" or a "Game.<anything>.png" belongs to it.
func screenshotFor(config *settings.Config, emulatorDir, savePath string) string {
	if config == nil || config.ScreenshotDir == "" || emulatorDir == "" {
		return ""
	}
	saved, err := os.Stat(savePath)
	if err != nil {
		return ""
	}
	dir := filepath.Join(config.ScreenshotDir, emulatorDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	base := filepath.Base(savePath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !screenshotBelongs(name, stem) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		gap := info.ModTime().Sub(saved.ModTime())
		if gap < 0 {
			gap = -gap
		}
		if gap <= screenshotFresh {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

// screenshotBelongs reports whether the picture named name is of the game
// whose save is named stem without its extension.
func screenshotBelongs(name, stem string) bool {
	if !strings.EqualFold(filepath.Ext(name), ".png") {
		return false
	}
	pictured := strings.TrimSuffix(name, filepath.Ext(name))
	if pictured == stem {
		return true
	}
	// "Game.sfc" for the save "Game.srm": the stem plus one more extension
	rest, ok := strings.CutPrefix(pictured, stem+".")
	return ok && rest != "" && !strings.ContainsAny(rest, ". ")
}
