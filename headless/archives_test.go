package headless

import "testing"

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
