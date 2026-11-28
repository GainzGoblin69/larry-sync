package saves

import (
	"testing"
	"time"

	"grout/romm"
)

// The newest of a slot is what the next sync of that slot brings down, so it
// decides whether a chosen save must be put back on the server as newest.
func TestNewestInSlot(t *testing.T) {
	at := func(days int) time.Time { return time.Date(2026, 11, 1+days, 0, 0, 0, 0, time.UTC) }
	slot := func(name string) *string { return &name }
	list := []romm.Save{
		{ID: 1, Slot: nil, UpdatedAt: at(1)},              // autosave, oldest
		{ID: 2, Slot: slot("autosave"), UpdatedAt: at(5)}, // autosave, newest
		{ID: 3, Slot: slot("slot2"), UpdatedAt: at(2)},    // only one in its slot
		{ID: 4, Slot: slot("slot2"), UpdatedAt: at(2)},    // same time as 3
	}
	want := map[int]bool{1: false, 2: true, 3: true, 4: true}
	for _, save := range list {
		if got := NewestInSlot(save, list); got != want[save.ID] {
			t.Errorf("NewestInSlot(save %d) = %v, want %v", save.ID, got, want[save.ID])
		}
	}
}
