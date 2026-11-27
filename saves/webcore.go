package saves

import (
	"path/filepath"
	"strings"
)

// webCores are the cores RomM's web player runs each platform with, by
// RomM's platform slug, first the one it uses. RomM keeps a game's saves
// apart by emulator name, and players that ask RomM for "the newer save"
// (RomM's web player, and the RomM TV app, which copies it) ask only about
// saves under these names. Saves sent under the same name are one save that
// every device carries on from. Kept in step with the TV app's WEB_CORES.
var webCores = map[string]string{
	"nes":                  "fceumm",
	"famicom":              "fceumm",
	"snes":                 "snes9x",
	"sfam":                 "snes9x",
	"gb":                   "gambatte",
	"gbc":                  "gambatte",
	"gba":                  "mgba",
	"n64":                  "mupen64plus_next",
	"nds":                  "melonds",
	"genesis":              "genesis_plus_gx",
	"sms":                  "genesis_plus_gx",
	"gamegear":             "genesis_plus_gx",
	"segacd":               "genesis_plus_gx",
	"psx":                  "pcsx_rearmed",
	"psp":                  "ppsspp",
	"tg16":                 "mednafen_pce",
	"turbografx-cd":        "mednafen_pce",
	"atari2600":            "stella2014",
	"atari7800":            "prosystem",
	"lynx":                 "handy",
	"neo-geo-pocket":       "mednafen_ngp",
	"neo-geo-pocket-color": "mednafen_ngp",
	"wonderswan":           "mednafen_wswan",
	"wonderswan-color":     "mednafen_wswan",
	"arcade":               "fbneo",
	"neogeoaes":            "fbneo",
	"neogeomvs":            "fbneo",
}

// rommEmulator is the emulator name a save goes to RomM under: the web
// player's core for its platform, so it is the same save the web player and
// the TV carry on from, or else the name of the emulator folder it is in.
func rommEmulator(fsSlug, emulatorDir string) string {
	if core, ok := webCores[strings.ToLower(fsSlug)]; ok {
		return core
	}
	emulator := filepath.Base(emulatorDir)
	if emulator == "." || emulator == "/" {
		return ""
	}
	if emulator == "SAVEDATA" { // PSP folder name varies across CFW
		return "PPSSPP"
	}
	return emulator
}
