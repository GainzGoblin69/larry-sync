package headless

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"grout/cfw"
	"grout/saves"
	"grout/settings"
)

// GameRef names a game by its RomM id or by the path of its file on the
// device. The in-game menu knows only the file it is playing.
type GameRef struct {
	RomID int
	Path  string
}

// ErrNoGame is returned when a GameRef names neither.
var ErrNoGame = errors.New("give --rom or --path")

// romID resolves ref to the game's id, finding a path among the games on the
// device the same way save sync does.
func (s *Session) romID(ref GameRef) (int, error) {
	if ref.RomID != 0 {
		return ref.RomID, nil
	}
	if ref.Path == "" {
		return 0, ErrNoGame
	}
	want := filepath.Clean(ref.Path)
	for id, rom := range saves.ResolveLocalRoms(cfw.ScanRoms(*s.Config)) {
		if filepath.Clean(rom.FilePath) == want {
			return id, nil
		}
	}
	for key, record := range s.State.Downloads {
		if filepath.Clean(record.Path) != want {
			continue
		}
		if id, err := strconv.Atoi(key); err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("%w: %s", saves.ErrRomNotOnDevice, filepath.Base(ref.Path))
}

// ListSaves reports each of a game's saves on the server as a SAVE line,
// newest first, with what a person choosing between them would want: when,
// which slot, how big, and whether it is the one this device has.
func (s *Session) ListSaves(ref GameRef) error {
	romID, err := s.romID(ref)
	if err != nil {
		return err
	}
	if s.Host.DeviceID == "" {
		return ErrNoDevice
	}
	list, err := saves.ServerSaves(s.client(), romID)
	if err != nil {
		return fmt.Errorf("listing saves: %w", err)
	}

	now := time.Now()
	for _, save := range list {
		onDevice, holders := "0", []string{}
		for _, sync := range save.DeviceSyncs {
			if !sync.IsCurrent {
				continue
			}
			if sync.DeviceID == s.Host.DeviceID {
				onDevice = "1"
			}
			if sync.DeviceName != "" {
				holders = append(holders, sync.DeviceName)
			}
		}
		newest := "0"
		if saves.NewestInSlot(save, list) {
			newest = "1"
		}
		s.Events.Save(
			strconv.Itoa(save.ID),
			saves.SlotName(save.Slot),
			strconv.FormatInt(save.UpdatedAt.Unix(), 10),
			Age(now.Sub(save.UpdatedAt)),
			strconv.FormatInt(save.FileSizeBytes, 10),
			newest,
			onDevice,
			strings.Join(holders, ", "),
			save.Emulator,
			save.FileName,
		)
	}
	s.Events.Result("rom", strconv.Itoa(romID))
	s.Events.Result("count", strconv.Itoa(len(list)))
	return nil
}

// ChooseSave puts one of a game's server saves on the device in place of the
// one there, which is backed up first, and keeps it chosen: see
// saves.ChooseSave. The device follows the save's slot from then on.
func (s *Session) ChooseSave(ref GameRef, saveID int) error {
	romID, err := s.romID(ref)
	if err != nil {
		return err
	}
	if s.Host.DeviceID == "" {
		return ErrNoDevice
	}

	s.Events.Stage("saves", "")
	chosen, err := saves.ChooseSave(s.client(), s.Config, s.Host.DeviceID, romID, saveID)
	if chosen.Path != "" {
		s.Events.Result("save_path", chosen.Path)
	}
	if err != nil {
		return err
	}

	// Following the slot keeps later syncs on the save that was picked. The
	// default slot needs no entry: it is followed when there is none.
	if chosen.Slot == settings.DefaultSaveSlot {
		s.Config.SetSlotPreference(romID, "")
	} else {
		s.Config.SetSlotPreference(romID, chosen.Slot)
	}
	if err := settings.SaveConfig(s.Config); err != nil {
		return fmt.Errorf("the save is loaded, but following its slot could not be saved: %w", err)
	}

	s.Events.Result("slot", chosen.Slot)
	s.Events.Result("promoted", map[bool]string{true: "1", false: "0"}[chosen.Promoted])
	return nil
}

// Age says how long ago something was, the way the RomM site does, for a
// screen with room for a few words.
func Age(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(n) + " " + unit + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d < 365*24*time.Hour:
		return plural(int(d/(30*24*time.Hour)), "month")
	default:
		return plural(int(d/(365*24*time.Hour)), "year")
	}
}
