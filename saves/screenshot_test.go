package saves

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"grout/settings"
)

func TestScreenshotBelongs(t *testing.T) {
	cases := []struct {
		name, stem string
		want       bool
	}{
		{"Game.sfc.png", "Game.sfc", true},
		{"Game.sfc.png", "Game", true},
		{"Game.sfc.PNG", "Game.sfc", true},
		{"Game 2.sfc.png", "Game", false},
		{"Game.part 2.png", "Game", false},
		{"Game.sfc.bmp", "Game.sfc", false},
		{"Gamey.sfc.png", "Game", false},
		{"Game.png", "Game", true},
	}
	for _, c := range cases {
		if got := screenshotBelongs(c.name, c.stem); got != c.want {
			t.Errorf("screenshotBelongs(%q, %q) = %v, want %v", c.name, c.stem, got, c.want)
		}
	}
}

func TestScreenshotFor(t *testing.T) {
	root := t.TempDir()
	saves := filepath.Join(root, "Saves", "SFC")
	shots := filepath.Join(root, "shots", "SFC")
	for _, dir := range []string{saves, shots} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	save := filepath.Join(saves, "Game.sfc.sav")
	shot := filepath.Join(shots, "Game.sfc.png")
	for _, path := range []string{save, shot} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	config := &settings.Config{ScreenshotDir: filepath.Join(root, "shots")}

	touch := func(path string, at time.Time) {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	touch(save, now)
	touch(shot, now.Add(-time.Minute))
	if got := screenshotFor(config, "SFC", save); got != shot {
		t.Errorf("fresh picture: got %q, want %q", got, shot)
	}

	touch(shot, now.Add(-time.Hour))
	if got := screenshotFor(config, "SFC", save); got != "" {
		t.Errorf("an hour-old picture went with the save: %q", got)
	}

	touch(shot, now)
	if got := screenshotFor(&settings.Config{}, "SFC", save); got != "" {
		t.Errorf("no picture folder set, got %q", got)
	}
	if got := screenshotFor(config, "GBA", save); got != "" {
		t.Errorf("another emulator's folder, got %q", got)
	}
}
