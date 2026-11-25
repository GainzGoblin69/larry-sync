package cache

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"grout/romm"
)

// knownFiles maps the name of a game file this device downloaded, without its
// extension and in lower case, to the RomM id it was downloaded as.
//
// Games are found by name: a file on the card is matched against the file
// names RomM lists. A game unpacked from an archive keeps whatever name it had
// inside the archive, which RomM never lists, so it would never match and its
// saves would never sync. The device's own record of what it downloaded says
// which game each such file is.
var (
	knownMu    sync.RWMutex
	knownFiles map[string]int
)

// SetKnownFiles replaces the record of downloaded game files: paths keyed by
// RomM id. It lives outside the Manager, which is reopened during a sync.
func SetKnownFiles(paths map[int]string) {
	known := make(map[string]int, len(paths))
	for id, path := range paths {
		if key := knownKey(path); key != "" && id > 0 {
			known[key] = id
		}
	}
	knownMu.Lock()
	knownFiles = known
	knownMu.Unlock()
}

func knownKey(path string) string {
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
}

func knownRomID(localBasename string) (int, bool) {
	knownMu.RLock()
	defer knownMu.RUnlock()
	id, ok := knownFiles[strings.ToLower(localBasename)]
	return id, ok
}

// knownLookup resolves localBasename from the record of downloaded files.
// The game must be on fsSlug's platform: the same name can be a game on two
// platforms, and a save must never go to the other one. Caller holds cm.mu.
func (cm *Manager) knownLookup(fsSlug, localBasename string) (romm.Rom, bool) {
	id, ok := knownRomID(localBasename)
	if !ok {
		return romm.Rom{}, false
	}
	var dataJSON string
	err := cm.db.QueryRow(`SELECT data_json FROM games WHERE id = ?`, id).Scan(&dataJSON)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			cm.stats.recordError()
		}
		return romm.Rom{}, false
	}
	var game romm.Rom
	if err := json.Unmarshal([]byte(dataJSON), &game); err != nil {
		return romm.Rom{}, false
	}
	if game.PlatformFSSlug != "" && !strings.EqualFold(game.PlatformFSSlug, fsSlug) {
		return romm.Rom{}, false
	}
	return game, true
}
