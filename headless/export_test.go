package headless

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"grout/cache"
	"grout/cfw"
	"grout/romm"
	"grout/settings"
)

// The catalog is a process-wide singleton rooted at the working directory, so
// everything that needs a real one lives in this single test.
func TestExportListsEveryGameAndMarksWhatIsOnTheDevice(t *testing.T) {
	card := t.TempDir()
	work := t.TempDir()
	t.Setenv("BASE_PATH", card)
	t.Setenv(cfw.EnvVar, string(cfw.NextUI))
	t.Setenv(ExportDirEnv, "")
	t.Chdir(work)

	const gbaFolder = "Game Boy Advance (GBA)"
	config := &settings.Config{
		Hosts: []settings.Host{{RootURI: "http://romm.invalid", Token: "token", DeviceID: "device"}},
		DirectoryMappings: map[string]settings.DirectoryMapping{
			"gba":  {RomMSlug: "gba", RelativePath: gbaFolder},
			"snes": {RomMSlug: "snes", RelativePath: "Super Nintendo Entertainment System (SFC)"},
		},
	}
	if err := settings.SaveConfig(config); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var out bytes.Buffer
	session, err := Open(NewEvents(&out))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cache.DeleteCacheFolder() })

	cm := cache.GetCacheManager()
	platforms := []romm.Platform{
		{ID: 1, Slug: "gba", FSSlug: "gba", Name: "Game Boy Advance", ROMCount: 2},
		{ID: 2, Slug: "snes", FSSlug: "snes", Name: "Super Nintendo", ROMCount: 1},
		// Mapped to nothing, so it must not be listed.
		{ID: 3, Slug: "ps5", FSSlug: "ps5", Name: "PlayStation 5", ROMCount: 1},
	}
	if err := cm.SavePlatforms(platforms); err != nil {
		t.Fatalf("saving platforms: %v", err)
	}

	gba := []romm.Rom{
		{
			ID: 11, PlatformID: 1, PlatformFSSlug: "gba", Name: "Wario Land 4",
			FsName: "Wario Land 4 (USA).gba", FsNameNoExt: "Wario Land 4 (USA)", FsSizeBytes: 8388608,
			Files: []romm.RomFile{{ID: 111, FileName: "Wario Land 4 (USA).gba"}},
		},
		{
			ID: 10, PlatformID: 1, PlatformFSSlug: "gba", Name: "advance Wars",
			FsName: "Advance Wars (USA).gba", FsNameNoExt: "Advance Wars (USA)", FsSizeBytes: 4194304,
			Files: []romm.RomFile{{ID: 110, FileName: "Advance Wars (USA).gba"}},
		},
	}
	if err := cm.SavePlatformGames(1, gba); err != nil {
		t.Fatalf("saving gba games: %v", err)
	}
	snes := []romm.Rom{{
		ID: 20, PlatformID: 2, PlatformFSSlug: "snes", Name: "Super Metroid",
		FsName: "Super Metroid (USA).sfc", FsNameNoExt: "Super Metroid (USA)", FsSizeBytes: 3145728,
		Files: []romm.RomFile{{ID: 120, FileName: "Super Metroid (USA).sfc"}},
	}}
	if err := cm.SavePlatformGames(2, snes); err != nil {
		t.Fatalf("saving snes games: %v", err)
	}

	// One of the three games is on the card.
	romPath := filepath.Join(card, "Roms", gbaFolder, "Wario Land 4 (USA).gba")
	if err := os.MkdirAll(filepath.Dir(romPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(romPath, []byte("rom"), 0644); err != nil {
		t.Fatal(err)
	}

	// A list left over from a platform that is no longer on the server.
	exportDir := filepath.Join(work, "larry")
	stale := filepath.Join(exportDir, GamesDirName, "n64.tsv")
	if err := os.MkdirAll(filepath.Dir(stale), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := session.Export(); err != nil {
		t.Fatalf("export: %v", err)
	}

	index := readLines(t, filepath.Join(exportDir, PlatformsFileName))
	wantIndex := []string{
		"gba\tGame Boy Advance\t" + filepath.Join(card, "Roms", gbaFolder) + "\t2\t1",
		"snes\tSuper Nintendo\t" + filepath.Join(card, "Roms", "Super Nintendo Entertainment System (SFC)") + "\t1\t0",
	}
	if strings.Join(index, "\n") != strings.Join(wantIndex, "\n") {
		t.Errorf("platforms.tsv =\n%s\nwant\n%s", strings.Join(index, "\n"), strings.Join(wantIndex, "\n"))
	}

	games := readLines(t, filepath.Join(exportDir, GamesDirName, "gba.tsv"))
	wantGames := []string{
		// Sorted without regard to case, so the lowercase name still comes first.
		"10\tadvance Wars\t0\t\t\t4194304\tAdvance Wars (USA).gba",
		"11\tWario Land 4\t1\t" + romPath + "\t\t8388608\tWario Land 4 (USA).gba",
	}
	if strings.Join(games, "\n") != strings.Join(wantGames, "\n") {
		t.Errorf("gba.tsv =\n%s\nwant\n%s", strings.Join(games, "\n"), strings.Join(wantGames, "\n"))
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the list for a platform that is gone should have been removed")
	}
	if !strings.Contains(out.String(), "RESULT\tdownloaded\t1\n") {
		t.Errorf("events should report one downloaded game, got:\n%s", out.String())
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}
