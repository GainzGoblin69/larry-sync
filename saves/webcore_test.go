package saves

import "testing"

func TestRommEmulator(t *testing.T) {
	cases := []struct{ slug, dir, want string }{
		{"snes", "/mnt/SDCARD/Saves/SFC", "snes9x"},
		{"SNES", "SFC", "snes9x"},
		{"gba", "GBA", "mgba"},
		{"gbc", "GBC", "gambatte"},
		{"psx", "PS", "pcsx_rearmed"},
		{"psp", "SAVEDATA", "ppsspp"},
		{"pico8", "/mnt/SDCARD/Saves/P8", "P8"},
		{"", "SAVEDATA", "PPSSPP"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := rommEmulator(c.slug, c.dir); got != c.want {
			t.Errorf("rommEmulator(%q, %q) = %q, want %q", c.slug, c.dir, got, c.want)
		}
	}
}
