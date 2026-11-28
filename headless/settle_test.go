package headless

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"grout/romm"
	"grout/saves"
)

// conflictAt makes a conflict between a save on the device last written at
// device and one on RomM last changed at server.
func conflictAt(t *testing.T, device, server time.Time) saves.SyncItem {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Super Mario World (USA).srm")
	if err := os.WriteFile(path, []byte("save"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, device, device); err != nil {
		t.Fatal(err)
	}
	return saves.SyncItem{
		LocalSave:  saves.LocalSave{RomID: 7, FilePath: path, FileName: filepath.Base(path)},
		RemoteSave: &romm.Save{ID: 42, RomID: 7, UpdatedAt: server},
		Action:     saves.ActionConflict,
	}
}

func TestSettleConflictsTakesTheNewerSave(t *testing.T) {
	now := time.Now()
	older, newer := now.Add(-time.Hour), now.Add(-time.Minute)
	cases := []struct {
		name      string
		device    time.Time
		server    time.Time
		direction Direction
		want      saves.Action
		force     bool
	}{
		{"the TV's save is newer, before a game", older, newer, DirectionPull, saves.ActionDownload, false},
		{"the TV's save is newer, a full sync", older, newer, DirectionBoth, saves.ActionDownload, false},
		{"the TV's save is newer, after a game", older, newer, DirectionPush, saves.ActionConflict, false},
		{"the device's save is newer, after a game", newer, older, DirectionPush, saves.ActionUpload, true},
		{"the device's save is newer, a full sync", newer, older, DirectionBoth, saves.ActionUpload, true},
		{"the device's save is newer, before a game", newer, older, DirectionPull, saves.ActionConflict, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := settleConflicts([]saves.SyncItem{conflictAt(t, c.device, c.server)}, c.direction)
			if got[0].Action != c.want || got[0].ForceOverwrite != c.force {
				t.Fatalf("action %s force %v, want %s force %v", got[0].Action, got[0].ForceOverwrite, c.want, c.force)
			}
		})
	}
}

func TestSettleConflictsLeavesOtherSavesAlone(t *testing.T) {
	items := settleConflicts([]saves.SyncItem{
		{Action: saves.ActionUpload},
		{Action: saves.ActionConflict}, // nothing known about RomM's copy
	}, DirectionBoth)
	if items[0].Action != saves.ActionUpload || items[1].Action != saves.ActionConflict {
		t.Fatalf("got %s and %s", items[0].Action, items[1].Action)
	}
}
