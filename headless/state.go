package headless

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"grout/files"
)

// stateFileName sits in the export directory beside the catalog files.
const stateFileName = "state.json"

// State is what the service has to remember between runs that grout's own
// config does not hold.
type State struct {
	// PlatformsBinding is the server's slug mapping as of the last sync. Grout
	// reads it from the server on every launch; keeping a copy lets the
	// catalog be written with the right folders while offline.
	PlatformsBinding map[string]string `json:"platforms_binding,omitempty"`
	// Downloads records where each fetched game landed, keyed by rom id.
	// Unpacking an archive moves the game to a name the server never listed,
	// so the path cannot always be worked out again from the catalog.
	Downloads map[string]DownloadRecord `json:"downloads,omitempty"`
	// LastSync is when the catalog was last refreshed from the server.
	LastSync time.Time `json:"last_sync,omitempty"`
}

// DownloadRecord is one game on the device.
type DownloadRecord struct {
	Path         string    `json:"path"`
	SizeBytes    int64     `json:"size_bytes,omitempty"`
	DownloadedAt time.Time `json:"downloaded_at"`
}

// LoadState reads the state file from dir. A missing file is an empty state.
func LoadState(dir string) (State, error) {
	var state State

	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

// Save writes the state file to dir.
func (s State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return files.WriteFileAtomic(filepath.Join(dir, stateFileName), data, 0644)
}

// RecordDownload notes where a game landed.
func (s *State) RecordDownload(romID int, path string, size int64) {
	if s.Downloads == nil {
		s.Downloads = make(map[string]DownloadRecord)
	}
	s.Downloads[strconv.Itoa(romID)] = DownloadRecord{
		Path:         path,
		SizeBytes:    size,
		DownloadedAt: time.Now().UTC(),
	}
}

// DownloadedPath is where a game was recorded as landing, or "" when it was
// never fetched or the file has since gone.
func (s State) DownloadedPath(romID int) string {
	record, ok := s.Downloads[strconv.Itoa(romID)]
	if !ok || record.Path == "" {
		return ""
	}
	if !files.FileExists(record.Path) {
		return ""
	}
	return record.Path
}
