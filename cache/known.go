package cache

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"grout/romm"
)

// SetKnownFiles replaces the record of downloaded game files: paths keyed by
// RomM id.
//
// Games are found by name: a file on the card is matched against the file
// names RomM lists. A game unpacked from an archive keeps whatever name it had
// inside the archive, which RomM never lists, so it would never match and its
// saves would never sync. The device's own record of what it downloaded says
// which game each such file is.
func (cm *Manager) SetKnownFiles(paths map[int]string) {
	if cm == nil {
		return
	}
	known := make(map[string]int, len(paths))
	for id, path := range paths {
		if key := knownKey(path); key != "" && id > 0 {
			known[key] = id
		}
	}
	cm.mu.Lock()
	cm.known = known
	cm.mu.Unlock()
}

func knownKey(path string) string {
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
}

// knownLookup resolves localBasename from the record of downloaded files.
// The game must be on fsSlug's platform: the same name can be a game on two
// platforms, and a save must never go to the other one. Caller holds cm.mu.
func (cm *Manager) knownLookup(fsSlug, localBasename string) (romm.Rom, bool) {
	id, ok := cm.known[strings.ToLower(localBasename)]
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
