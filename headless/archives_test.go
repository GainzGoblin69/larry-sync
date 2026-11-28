package headless

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"grout/romm"
)

// NextUI's emulator opens no archives, so only arcade folders keep zips.
func TestKeepsArchives(t *testing.T) {
	tests := map[string]bool{
		"/mnt/SDCARD/Roms/Arcade (FBN)":                              true,
		"/mnt/SDCARD/Roms/Arcade (fbneo)":                            true,
		"/mnt/SDCARD/Roms/MAME 2003 (MAME)":                          true,
		"/mnt/SDCARD/Roms/Super Nintendo Entertainment System (SFC)": false,
		"/mnt/SDCARD/Roms/Neo Geo Pocket Color (NGPC)":               false,
		"/mnt/SDCARD/Roms/Game Boy Advance (GBA)/":                   false,
		"/mnt/SDCARD/Roms/Arcade":                                    false,
		"/mnt/SDCARD/Roms/Odd) (Name":                                false,
		"":                                                           false,
	}
	for dir, want := range tests {
		if got := keepsArchives(dir); got != want {
			t.Errorf("keepsArchives(%q) = %v, want %v", dir, got, want)
		}
	}
}

// A game unpacked from an archive takes the name RomM knows it by, or save
// sync, which finds games by name, never fetches its save.
func TestNameAfterRomm(t *testing.T) {
	dkc3 := romm.Rom{
		FsName:      "DKC3.zip",
		FsNameNoExt: "DKC3",
		Files:       []romm.RomFile{{FileName: "DKC3.zip"}},
	}

	t.Run("renames to RomM's name", func(t *testing.T) {
		dir := t.TempDir()
		inner := filepath.Join(dir, "Donkey Kong Country 3 (USA).sfc")
		writeFile(t, inner)
		got := nameAfterRomm(dkc3, inner, dir)
		if want := filepath.Join(dir, "DKC3.sfc"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("renamed file missing: %v", err)
		}
		if _, err := os.Stat(inner); !os.IsNotExist(err) {
			t.Fatal("the old name should be gone")
		}
	})

	t.Run("moves a game out of a folder in the archive", func(t *testing.T) {
		dir := t.TempDir()
		inner := filepath.Join(dir, "DKC3 (USA)", "game.sfc")
		writeFile(t, inner)
		if got := nameAfterRomm(dkc3, inner, dir); got != filepath.Join(dir, "DKC3.sfc") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("leaves a name RomM already knows", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "DKC3.sfc")
		writeFile(t, path)
		if got := nameAfterRomm(dkc3, path, dir); got != path {
			t.Fatalf("got %q, want it unchanged", got)
		}
	})

	t.Run("never overwrites", func(t *testing.T) {
		dir := t.TempDir()
		inner := filepath.Join(dir, "Other.sfc")
		writeFile(t, inner)
		writeFile(t, filepath.Join(dir, "DKC3.sfc"))
		if got := nameAfterRomm(dkc3, inner, dir); got != inner {
			t.Fatalf("got %q, want it left alone", got)
		}
	})

	t.Run("leaves playlists and multi-file games", func(t *testing.T) {
		dir := t.TempDir()
		playlist := filepath.Join(dir, "Game.m3u")
		writeFile(t, playlist)
		if got := nameAfterRomm(dkc3, playlist, dir); got != playlist {
			t.Fatalf("a playlist was renamed to %q", got)
		}
		multi := dkc3
		multi.HasMultipleFiles = true
		other := filepath.Join(dir, "Other.sfc")
		writeFile(t, other)
		if got := nameAfterRomm(multi, other, dir); got != other {
			t.Fatalf("a multi-file game was renamed to %q", got)
		}
	})

	t.Run("refuses a name from the server that leaves the folder", func(t *testing.T) {
		dir := t.TempDir()
		inner := filepath.Join(dir, "x.sfc")
		writeFile(t, inner)
		evil := romm.Rom{FsNameNoExt: "../escaped", Files: []romm.RomFile{{FileName: "../escaped.zip"}}}
		if got := nameAfterRomm(evil, inner, dir); got != inner {
			t.Fatalf("got %q, want it left inside the folder", got)
		}
	})
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rom"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestAge(t *testing.T) {
	tests := map[time.Duration]string{
		10 * time.Second:     "just now",
		time.Minute:          "1 minute ago",
		59 * time.Minute:     "59 minutes ago",
		3 * time.Hour:        "3 hours ago",
		9 * 24 * time.Hour:   "9 days ago",
		45 * 24 * time.Hour:  "1 month ago",
		800 * 24 * time.Hour: "2 years ago",
	}
	for d, want := range tests {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}
