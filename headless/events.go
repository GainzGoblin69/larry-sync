// Package headless runs grout's catalog, download, BIOS and save sync without a
// screen, for a launcher that drives them as a child process.
//
// Every command reports through Events as tab-separated lines, which is what a
// C frontend can read without a JSON parser. Nothing here draws, prompts or
// exits; cmd/larry-sync owns the process.
package headless

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Event kinds, one per line, fields separated by tabs.
const (
	// KindStage starts a named step: STAGE <name> <detail>.
	KindStage = "STAGE"
	// KindProgress reports the current step: PROGRESS <percent> <done> <total>.
	// done and total are bytes for transfers and item counts otherwise; total
	// is 0 when it is not known.
	KindProgress = "PROGRESS"
	// KindInfo is a line worth showing but not acting on: INFO <message>.
	KindInfo = "INFO"
	// KindResult is an outcome the caller reads back: RESULT <key> <value>.
	KindResult = "RESULT"
	// KindSave is one of a game's saves on the server, from list-saves:
	// SAVE <id> <slot> <updated, unix seconds> <age> <bytes> <newest in slot,
	// 1 or 0> <on this device, 1 or 0> <devices holding it> <emulator> <file>.
	KindSave = "SAVE"
	// KindError is why the command failed: ERROR <message>.
	KindError = "ERROR"
)

// Events writes the line protocol. It is safe for concurrent use, since
// downloads report from their own goroutines.
type Events struct {
	mu sync.Mutex
	w  io.Writer
	// lastPercent suppresses repeats, so a large download is a hundred lines
	// rather than one per network read.
	lastPercent int
}

// NewEvents returns an Events writing to w.
func NewEvents(w io.Writer) *Events {
	return &Events{w: w, lastPercent: -1}
}

// Stage announces a new step and resets progress for it.
func (e *Events) Stage(name, detail string) {
	e.mu.Lock()
	e.lastPercent = -1
	e.mu.Unlock()
	e.emit(KindStage, name, detail)
}

// Progress reports how far the current step is. Calls that would repeat the
// last whole percentage are dropped.
func (e *Events) Progress(done, total int64) {
	percent := 0
	if total > 0 {
		percent = int(done * 100 / total)
		if percent > 100 {
			percent = 100
		}
	}

	e.mu.Lock()
	if percent == e.lastPercent {
		e.mu.Unlock()
		return
	}
	e.lastPercent = percent
	e.mu.Unlock()

	e.emit(KindProgress, strconv.Itoa(percent), strconv.FormatInt(done, 10), strconv.FormatInt(total, 10))
}

// Info reports something a person might want to read.
func (e *Events) Info(format string, args ...any) {
	e.emit(KindInfo, fmt.Sprintf(format, args...))
}

// Result reports a named outcome.
func (e *Events) Result(key, value string) {
	e.emit(KindResult, key, value)
}

// Save reports one of a game's saves on the server, for list-saves.
func (e *Events) Save(fields ...string) {
	e.emit(KindSave, fields...)
}

// Error reports why a command failed.
func (e *Events) Error(err error) {
	if err == nil {
		return
	}
	e.emit(KindError, err.Error())
}

func (e *Events) emit(kind string, fields ...string) {
	parts := make([]string, 0, len(fields)+1)
	parts = append(parts, kind)
	for _, field := range fields {
		parts = append(parts, cleanField(field))
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Fprintln(e.w, strings.Join(parts, "\t"))
}

// cleanField keeps a value on one line and in one column. Game names come from
// the server and can hold anything.
func cleanField(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
}
