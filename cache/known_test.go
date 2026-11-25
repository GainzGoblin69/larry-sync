package cache

import (
	"testing"

	"grout/romm"
)

// A game unpacked from an archive keeps the name it had inside the archive,
// which RomM never lists. The device's record of what it downloaded must
// still find it, on its own platform only.
func TestGetRomByFSLookup_FindsDownloadedFileByRecord(t *testing.T) {
	cm := newTestManager(t)
	snes := romm.Rom{
		ID:             310,
		PlatformID:     2,
		PlatformFSSlug: "snes",
		Name:           "Donkey Kong Country 3: Dixie Kong's Double Trouble!",
		FsName:         "Donkey Kong Country 3 - Dixie Kong's Double Trouble!.zip",
		FsNameNoExt:    "Donkey Kong Country 3 - Dixie Kong's Double Trouble!",
		Files:          []romm.RomFile{{FileName: "Donkey Kong Country 3 - Dixie Kong's Double Trouble!.zip"}},
	}
	gba := romm.Rom{
		ID:             311,
		PlatformID:     5,
		PlatformFSSlug: "gba",
		Name:           "Donkey Kong Country 3",
		FsName:         "Donkey Kong Country 3.gba",
		FsNameNoExt:    "Donkey Kong Country 3",
		Files:          []romm.RomFile{{FileName: "Donkey Kong Country 3.gba"}},
	}
	if err := cm.SavePlatformGames(2, []romm.Rom{snes}); err != nil {
		t.Fatalf("save games: %v", err)
	}
	if err := cm.SavePlatformGames(5, []romm.Rom{gba}); err != nil {
		t.Fatalf("save games: %v", err)
	}

	unpacked := "Donkey Kong Country 3 - Dixie Kong's Double Trouble! (USA) (En,Fr)"
	t.Cleanup(func() { SetKnownFiles(nil) })

	if _, err := cm.GetRomByFSLookup("snes", unpacked); err == nil {
		t.Fatal("matched before the download was recorded; the test name is not a real miss")
	}

	SetKnownFiles(map[int]string{
		310: "/mnt/SDCARD/Roms/Super Nintendo Entertainment System (SFC)/" + unpacked + ".sfc",
	})

	got, err := cm.GetRomByFSLookup("snes", unpacked)
	if err != nil {
		t.Fatalf("recorded download not found: %v", err)
	}
	if got.ID != 310 {
		t.Errorf("got rom %d, want 310", got.ID)
	}
	// The save scan asks with the extension still on, then without; either
	// way the case of the name must not matter.
	if got, err := cm.GetRomByFSLookup("SNES", "donkey kong country 3 - dixie kong's double trouble! (usa) (en,fr)"); err != nil || got.ID != 310 {
		t.Errorf("case-insensitive lookup: got %d, %v", got.ID, err)
	}
	if _, err := cm.GetRomByFSLookup("gba", unpacked); err == nil {
		t.Error("the SNES download matched on the GBA platform")
	}
}

func TestKnownKey(t *testing.T) {
	cases := map[string]string{
		"/a/b/Game (USA).sfc":  "game (usa)",
		"Game.Name.v1.1.gba":   "game.name.v1.1",
		"/":                    "",
		"":                     "",
		"/roms/Arcade/sf2.zip": "sf2",
	}
	for in, want := range cases {
		if got := knownKey(in); got != want {
			t.Errorf("knownKey(%q) = %q, want %q", in, got, want)
		}
	}
}
