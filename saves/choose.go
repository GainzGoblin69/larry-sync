package saves

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"grout/cfw"
	"grout/files"
	"grout/romm"
	"grout/settings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
)

// ErrRomNotOnDevice is returned when a save is asked for a game this device
// does not have: there is nothing to put the save beside.
var ErrRomNotOnDevice = errors.New("the game is not on this device")

// ErrSaveNotFound is returned when the chosen save is not among the game's
// saves on the server, as when it was deleted in the meantime.
var ErrSaveNotFound = errors.New("that save is no longer on the server")

// ServerSaves lists a game's saves on the server, newest first. Asking as
// deviceID has the server say, in each save's DeviceSyncs, which one that
// device holds; it does not narrow the list.
func ServerSaves(client *romm.Client, romID int, deviceID string) ([]romm.Save, error) {
	all, err := client.GetSaves(romm.SaveQuery{RomID: romID, DeviceID: deviceID})
	if err != nil {
		return nil, err
	}
	list := make([]romm.Save, 0, len(all))
	for _, save := range all {
		if save.RomID == romID || save.RomID == 0 {
			list = append(list, save)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	return list, nil
}

// NewestInSlot reports whether save is the newest of saves in its own slot,
// which is the one the next sync of that slot would bring down.
func NewestInSlot(save romm.Save, saves []romm.Save) bool {
	slot := SlotName(save.Slot)
	for _, other := range saves {
		if other.ID != save.ID && SlotName(other.Slot) == slot && other.UpdatedAt.After(save.UpdatedAt) {
			return false
		}
	}
	return true
}

// ChosenSave is what choosing a save did.
type ChosenSave struct {
	// Path is where the save now is on the device.
	Path string
	// Slot is the slot the device now follows for the game.
	Slot string
	// Promoted is set when the save was older than another in its slot and
	// was put back on the server as that slot's newest, so the next sync
	// keeps it rather than bringing the newer one down over it.
	Promoted bool
}

// ChooseSave makes one of a game's saves on the server the one this device
// plays, as a person picking it by hand would expect: it replaces the save on
// the device, after backing that up, and it stays chosen.
//
// Staying chosen takes two things. The device follows the save's slot from
// now on. And a save older than the newest in its slot is uploaded again as a
// new version, so it becomes the newest: otherwise the next sync would bring
// the newer one straight back down. That upload carries no device and asks
// for no clean-up, so nothing already on the server is replaced or removed;
// it is the same as using Upload on the RomM site.
func ChooseSave(client *romm.Client, config *settings.Config, deviceID string, romID, saveID int) (ChosenSave, error) {
	logger := gaba.GetLogger()

	all, err := ServerSaves(client, romID, deviceID)
	if err != nil {
		return ChosenSave{}, fmt.Errorf("listing the game's saves: %w", err)
	}
	var chosen *romm.Save
	for i := range all {
		if all[i].ID == saveID {
			chosen = &all[i]
			break
		}
	}
	if chosen == nil {
		return ChosenSave{}, ErrSaveNotFound
	}

	rom, ok := ResolveLocalRoms(cfw.ScanRoms(*config))[romID]
	if !ok {
		return ChosenSave{}, ErrRomNotOnDevice
	}

	local := localSaveFor(config, rom)
	slot := SlotName(chosen.Slot)
	item := SyncItem{LocalSave: local, RemoteSave: chosen, Action: ActionDownload, TargetSlot: slot}
	if !download(client, config, deviceID, &item) {
		return ChosenSave{}, errors.New("the save could not be brought down; the log on the card says why")
	}
	path := item.LocalSave.FilePath
	result := ChosenSave{Path: path, Slot: slot}

	if !NewestInSlot(*chosen, all) {
		promoted, err := promote(client, deviceID, item, path, slot)
		if err != nil {
			// The save is on the device either way; only keeping it chosen
			// failed, and that is worth saying rather than hiding.
			return result, fmt.Errorf("the save is loaded, but could not be made the newest in its slot, so the next sync may replace it: %w", err)
		}
		result.Promoted = true
		logger.Info("Chosen save made the newest in its slot", "rom", romID, "chosen", saveID, "uploaded_as", promoted.ID)
	}

	logger.Info("Save chosen", "rom", romID, "save", saveID, "slot", slot, "path", path, "promoted", result.Promoted)
	return result, nil
}

// localSaveFor is the device's save for rom, or, when it has none, where one
// would go: the same place a sync would put it.
func localSaveFor(config *settings.Config, rom cfw.LocalRomFile) LocalSave {
	for _, existing := range ScanSaves(config) {
		if existing.RomID == rom.RomID && existing.FilePath != "" {
			return existing
		}
	}
	local := LocalSave{
		RomID:       rom.RomID,
		RomName:     rom.RomName,
		FSSlug:      rom.FSSlug,
		RomFileName: rom.FileName,
		EmulatorDir: resolveDiscoveredSaveDirectory(rom, config),
	}
	local.IsDirectorySave = IsDirectorySavePlatform(local.FSSlug)
	return local
}

// promote uploads the save at path as a new version in slot, then records it
// as what this device holds, so sync sees the device and the server agree.
//
// It goes up under a name of its own, the save's name with the time in
// brackets as RomM names versions. An upload with no device and a name the
// server already holds replaces that save, and a restore must never do that.
func promote(client *romm.Client, deviceID string, item SyncItem, path, slot string) (romm.Save, error) {
	if item.LocalSave.IsDirectorySave {
		return romm.Save{}, errors.New("folder saves cannot be restored this way yet")
	}

	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	staging, err := os.MkdirTemp("", "larry-restore-*")
	if err != nil {
		return romm.Save{}, err
	}
	defer os.RemoveAll(staging)
	versioned := filepath.Join(staging, fmt.Sprintf("%s [%s]%s", base, time.Now().UTC().Format("2006-01-02_15-04-05"), ext))
	if err := files.CopyFile(path, versioned); err != nil {
		return romm.Save{}, err
	}

	emulator := filepath.Base(item.LocalSave.EmulatorDir)
	if emulator == "." || emulator == "" {
		emulator = "unknown"
	}
	// the chosen save's picture goes up with its new version
	picture := ""
	if item.RemoteSave != nil && item.RemoteSave.ScreenshotPath() != "" {
		if data, err := client.DownloadSaveScreenshot(*item.RemoteSave); err == nil {
			ext := item.RemoteSave.ScreenshotExt()
			if ext == "" {
				ext = ".png"
			}
			candidate := filepath.Join(staging, "picture"+ext)
			if os.WriteFile(candidate, data, 0o644) == nil {
				picture = candidate
			}
		} else {
			gaba.GetLogger().Info("Could not fetch the chosen save's picture", "save", item.RemoteSave.ID, "error", err)
		}
	}
	uploaded, err := client.UploadSaveWithScreenshot(romm.UploadSaveQuery{
		RomID:    item.LocalSave.RomID,
		Slot:     slot,
		Emulator: emulator,
	}, versioned, picture)
	if err != nil {
		return romm.Save{}, err
	}

	if err := client.ConfirmSaveDownloaded(uploaded.ID, deviceID); err != nil {
		gaba.GetLogger().Warn("Could not mark the restored save as this device's", "save", uploaded.ID, "error", err)
	}

	t := uploaded.UpdatedAt.Truncate(time.Second)
	if err := os.Chtimes(path, t, t); err != nil {
		gaba.GetLogger().Warn("Failed to set save mtime after restoring", "path", path, "error", err)
	}
	hash, _ := saveContentHash(LocalSave{FilePath: path})
	recordSaveState(deviceID, item.LocalSave.RomID, recordedDownloadFileName(item, path), slot, uploaded.ID, hash)
	return uploaded, nil
}
