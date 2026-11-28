package headless

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"grout/saves"
)

// Direction limits which way saves move.
type Direction string

const (
	// DirectionBoth uploads and downloads, which is what a sync after playing
	// wants.
	DirectionBoth Direction = "both"
	// DirectionPull only downloads, for bringing a save down before a game
	// starts.
	DirectionPull Direction = "pull"
	// DirectionPush only uploads.
	DirectionPush Direction = "push"
)

// ParseDirection reads a direction as typed on the command line.
func ParseDirection(s string) (Direction, error) {
	switch Direction(s) {
	case "", DirectionBoth:
		return DirectionBoth, nil
	case DirectionPull:
		return DirectionPull, nil
	case DirectionPush:
		return DirectionPush, nil
	}
	return "", fmt.Errorf("unknown direction %q; use both, pull or push", s)
}

// SaveOptions selects which saves a sync touches.
type SaveOptions struct {
	// RomID limits the sync to one game. Zero syncs every save on the device.
	RomID int
	// Direction limits which way saves move.
	Direction Direction
}

// ErrNoDevice means the server has not issued this device an id, which save
// sync is addressed by. Pairing again registers it.
var ErrNoDevice = errors.New("this device is not registered for save sync")

// SyncSaves reconciles saves with the server.
//
// The server decides what each save needs, exactly as it does for grout.
// A save both sides changed goes the way of the newer one (see
// settleConflicts), so a game always carries on from its latest save, and
// the other copy is kept rather than lost. A sync of every save also carries
// whatever was played while offline, so there is no separate queue to keep.
func (s *Session) SyncSaves(options SaveOptions) (saves.SyncReport, error) {
	if err := s.Online(); err != nil {
		return saves.SyncReport{}, err
	}
	return s.syncSaves(options)
}

// syncSaves is SyncSaves for a caller that has already checked the server
// answers.
func (s *Session) syncSaves(options SaveOptions) (saves.SyncReport, error) {
	if s.Host.DeviceID == "" {
		return saves.SyncReport{}, ErrNoDevice
	}

	s.Events.Stage("saves", "")
	client := s.client()

	result, err := saves.ResolveSaveSync(client, s.Config, s.Host.DeviceID)
	if err != nil {
		return saves.SyncReport{}, fmt.Errorf("comparing saves with the server: %w", err)
	}

	items := settleConflicts(selectSaveItems(result.Items, options), options.Direction)

	// What was decided, at info level, so a save that did not come down can
	// be traced from the log on the card without turning on debug logging.
	logger := slog.Default()
	logger.Info("Save check", "rom", options.RomID, "direction", options.Direction,
		"server_items", len(result.Items), "for_this_request", len(items))
	if options.RomID != 0 && len(items) == 0 {
		logger.Info("Nothing to sync for this game: it is up to date, has no save on RomM, or was not recognised on the device by name",
			"rom", options.RomID)
	}
	for _, item := range items {
		remote := ""
		if item.RemoteSave != nil {
			remote = item.RemoteSave.FileName
		}
		logger.Info("Save action", "rom", saveRomID(item), "action", item.Action.String(),
			"rom_file", item.LocalSave.RomFileName, "local_save", item.LocalSave.FilePath, "server_save", remote)
	}

	report := saves.ExecuteSaveSync(client, s.Config, s.Host.DeviceID, items, result.SessionID,
		func(current, total int) { s.Events.Progress(int64(current), int64(total)) })
	logger.Info("Save check done", "rom", options.RomID, "downloaded", report.Downloaded,
		"uploaded", report.Uploaded, "conflicts", report.Conflicts, "errors", report.Errors)

	s.Events.Result("saves_uploaded", strconv.Itoa(report.Uploaded))
	s.Events.Result("saves_downloaded", strconv.Itoa(report.Downloaded))
	s.Events.Result("saves_conflicts", strconv.Itoa(report.Conflicts))
	s.Events.Result("saves_errors", strconv.Itoa(report.Errors))
	// which games' saves went up, for the device to say so
	for _, item := range report.Items {
		if item.Action == saves.ActionUpload && item.Success {
			name := item.LocalSave.RomName
			if name == "" {
				name = item.LocalSave.FileName
			}
			s.Events.Result("uploaded_game", name)
		}
	}
	return report, nil
}

// selectSaveItems keeps the items a sync was asked to act on.
//
// Conflicts for the chosen games are kept whatever the direction, so they are
// still counted and reported rather than silently dropped.
func selectSaveItems(items []saves.SyncItem, options SaveOptions) []saves.SyncItem {
	kept := make([]saves.SyncItem, 0, len(items))
	for _, item := range items {
		if options.RomID != 0 && saveRomID(item) != options.RomID {
			continue
		}
		switch {
		case options.Direction == DirectionPull && item.Action == saves.ActionUpload:
			continue
		case options.Direction == DirectionPush && item.Action == saves.ActionDownload:
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// saveRomID is the game a sync item belongs to. An item for a save that only
// exists on the server has no local side to read it from.
func saveRomID(item saves.SyncItem) int {
	if item.LocalSave.RomID != 0 {
		return item.LocalSave.RomID
	}
	if item.RemoteSave != nil {
		return item.RemoteSave.RomID
	}
	return 0
}

// settleConflicts settles each save both sides changed in favour of the newer
// one: the player always carries on from their latest save, wherever it was
// made. Nothing is lost by it. A download first backs up the save it
// replaces (in the save folder's .backup); an upload adds a version on RomM,
// which keeps the ones before. A conflict the direction does not allow (the
// device's save is newer, before a game) stays, and the newer save on the
// device is what plays.
func settleConflicts(items []saves.SyncItem, direction Direction) []saves.SyncItem {
	logger := slog.Default()
	for i := range items {
		item := &items[i]
		if item.Action != saves.ActionConflict || item.RemoteSave == nil {
			continue
		}
		var local time.Time
		if info, err := os.Stat(item.LocalSave.FilePath); err == nil {
			local = info.ModTime()
		}
		server := item.RemoteSave.UpdatedAt
		switch {
		case !server.IsZero() && server.After(local) && item.RemoteSave.ID != 0:
			if direction == DirectionPush {
				continue
			}
			item.Action = saves.ActionDownload
		case !local.IsZero() && !server.After(local):
			if direction == DirectionPull {
				continue
			}
			item.Action = saves.ActionUpload
			item.ForceOverwrite = true
		default:
			continue
		}
		logger.Info("Save changed on both sides: the newer one wins", "rom", saveRomID(*item),
			"action", item.Action.String(), "device_save_time", local, "romm_save_time", server)
	}
	return items
}
