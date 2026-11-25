//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rommapi "grout/romm"
	"grout/saves"
)

// larry-sync is the screenless build of grout that Larry Boy Advanced runs on
// NextUI. These tests run it as the launcher does, one command at a time,
// against the same RomM as the UI tests, on a card laid out like NextUI's.

// nextUICard is a NextUI card holding the folders a fresh install has for the
// consoles named, by their NextUI folder names.
func nextUICard(t *testing.T, romFolders ...string) string {
	t.Helper()
	card := t.TempDir()
	for _, dir := range append([]string{"Bios", "Saves", ".userdata/shared"}, romFolders...) {
		if err := os.MkdirAll(filepath.Join(card, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return card
}

// larryHome is where the launcher's wrapper runs larry-sync from, holding a
// config paired with server under deviceID, as linking leaves it.
func larryHome(t *testing.T, card string, server *server, deviceID string) string {
	t.Helper()
	home := filepath.Join(card, ".userdata", "shared", "larry")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"hosts": []map[string]any{{
			"root_uri":    server.URL,
			"username":    server.Username,
			"token":       server.Token,
			"device_id":   deviceID,
			"device_name": "larry-e2e",
		}},
		"log_level": "debug",
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// run is one larry-sync command and what it printed.
type run struct {
	code   int
	events []string // stdout: the lines the launcher reads
	stderr string
}

func (r run) result(key string) string {
	for _, line := range r.events {
		if fields := strings.Split(line, "\t"); len(fields) >= 3 && fields[0] == "RESULT" && fields[1] == key {
			return fields[2]
		}
	}
	return ""
}

func larrySync(t *testing.T, card, home string, args ...string) run {
	t.Helper()
	cmd := exec.Command(os.Getenv("LARRY_SYNC_BINARY"), args...)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"CFW=NEXTUI",
		"NEXTUI_DEVICE=tg5040",
		"BASE_PATH="+card,
		"LARRY_DIR="+filepath.Join(home, "catalog"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting larry-sync: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("larry-sync %v did not finish\nstdout:\n%s\nstderr:\n%s", args, indent(stdout.String()), indent(stderr.String()))
	}

	r := run{code: cmd.ProcessState.ExitCode(), stderr: stderr.String()}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			r.events = append(r.events, line)
		}
	}
	return r
}

// explain is what a failed larry-sync test prints: everything it said, its
// log, and what is on the card.
func explain(t *testing.T, card, home string, runs ...run) string {
	var b strings.Builder
	for i, r := range runs {
		fmt.Fprintf(&b, "run %d exited %d\nstdout:\n%s\nstderr:\n%s\n", i+1, r.code, indent(strings.Join(r.events, "\n")), indent(r.stderr))
	}
	if log, err := os.ReadFile(filepath.Join(home, "logs", "larry-sync.log")); err == nil {
		fmt.Fprintf(&b, "larry-sync.log:\n%s\n", indent(string(log)))
	}
	b.WriteString("card holds:\n")
	_ = filepath.Walk(card, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.Contains(path, "/catalog/") && !strings.HasSuffix(path, ".db-shm") && !strings.HasSuffix(path, ".db-wal") {
			rel, _ := filepath.Rel(card, path)
			fmt.Fprintf(&b, "  %s (%d bytes)\n", rel, info.Size())
		}
		return nil
	})
	return b.String()
}

func client(server *server) *rommapi.Client {
	return rommapi.NewClient(server.URL, rommapi.WithAuthHeader("Bearer "+server.Token), rommapi.WithTimeout(30*time.Second))
}

func romNamed(t *testing.T, c *rommapi.Client, fileName string) rommapi.Rom {
	t.Helper()
	page, err := c.GetRoms(rommapi.GetRomsQuery{Search: strings.TrimSuffix(fileName, filepath.Ext(fileName)), Limit: 50})
	if err != nil {
		t.Fatalf("looking up %s: %v", fileName, err)
	}
	for _, rom := range page.Items {
		if rom.FsName == fileName {
			return rom
		}
	}
	t.Fatalf("RomM has no rom %q", fileName)
	return rommapi.Rom{}
}

// uploadSave puts a save on the server for rom with no device attached, the
// way the RomM website, or another emulator talking to RomM, stores one.
func uploadSave(t *testing.T, c *rommapi.Client, rom rommapi.Rom, fileName, content string) rommapi.Save {
	t.Helper()
	path := filepath.Join(t.TempDir(), fileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	save, err := c.UploadSaveWithQuery(rommapi.UploadSaveQuery{RomID: rom.ID, Emulator: "retroarch"}, path)
	if err != nil {
		t.Fatalf("uploading %s: %v", fileName, err)
	}
	return save
}

// A game with a save on RomM, fetched to a device that has never had it,
// starts from that save: it is written where NextUI's emulator reads it.
// This is the Hammer picking a game for the first time.
func TestLarrySyncBringsDownTheSaveOfANewlyFetchedGame(t *testing.T) {
	server := romm(t)
	c := client(server)

	// A device of its own, registered as linking registers the Hammer, so no
	// save another test synced can stand in for the one under test.
	device, err := saves.RegisterDevice(c, fmt.Sprintf("larry-e2e-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("registering a device: %v", err)
	}

	const romFile = "Another Game (Europe).gba"
	rom := romNamed(t, c, romFile)
	content := fmt.Sprintf("progress from another device %d", time.Now().UnixNano())
	uploaded := uploadSave(t, c, rom, "Another Game (Europe).srm", content)
	t.Logf("server save %d: %s slot=%v", uploaded.ID, uploaded.FileName, uploaded.Slot)

	card := nextUICard(t, "Roms/Game Boy Advance (GBA)")
	home := larryHome(t, card, server, device.ID)

	sync := larrySync(t, card, home, "sync", "--no-art")
	if sync.code != 0 {
		t.Fatalf("sync failed\n%s", explain(t, card, home, sync))
	}
	fetch := larrySync(t, card, home, "fetch", "--rom", fmt.Sprint(rom.ID))
	if fetch.code != 0 {
		t.Fatalf("fetch failed\n%s", explain(t, card, home, sync, fetch))
	}

	romPath := fetch.result("rom_path")
	if _, err := os.Stat(romPath); err != nil {
		t.Fatalf("the game is not where fetch said: %q\n%s", romPath, explain(t, card, home, sync, fetch))
	}

	// NextUI's default save format: the rom's whole file name, then .sav.
	savePath := filepath.Join(card, "Saves", "GBA", filepath.Base(romPath)+".sav")
	got, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("no save at %s (saves_downloaded=%q)\n%s", savePath, fetch.result("saves_downloaded"), explain(t, card, home, sync, fetch))
	}
	if string(got) != content {
		t.Fatalf("the save at %s holds %q, want the server's %q\n%s", savePath, got, content, explain(t, card, home, sync, fetch))
	}
}

// saveLines are the SAVE lines of a list-saves run, by save id.
func saveLines(r run) map[string][]string {
	out := map[string][]string{}
	for _, line := range r.events {
		if fields := strings.Split(line, "\t"); len(fields) >= 11 && fields[0] == "SAVE" {
			out[fields[1]] = fields[1:]
		}
	}
	return out
}

// Choosing an older save from RomM in the in-game menu puts it on the device,
// backs up the one that was there, and keeps it: the check before the next
// play must not bring the newer one straight back down over it.
func TestLarrySyncChoosesAnOlderSaveAndKeepsIt(t *testing.T) {
	server := romm(t)
	c := client(server)
	device, err := saves.RegisterDevice(c, fmt.Sprintf("larry-e2e-choose-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("registering a device: %v", err)
	}

	const romFile = "Another Game (Europe).gba"
	rom := romNamed(t, c, romFile)
	stamp := time.Now().UnixNano()
	older := fmt.Sprintf("older progress %d", stamp)
	newer := fmt.Sprintf("newer progress %d", stamp)
	// Named apart, as saves from devices are: RomM replaces a save uploaded
	// again under the same name rather than keeping both.
	a := uploadSave(t, c, rom, fmt.Sprintf("Another Game (Europe) [older %d].srm", stamp), older)
	time.Sleep(1100 * time.Millisecond) // RomM keeps whole seconds
	b := uploadSave(t, c, rom, fmt.Sprintf("Another Game (Europe) [newer %d].srm", stamp), newer)
	if a.ID == b.ID {
		t.Fatalf("RomM gave both saves id %d", a.ID)
	}

	card := nextUICard(t, "Roms/Game Boy Advance (GBA)")
	home := larryHome(t, card, server, device.ID)
	runs := []run{larrySync(t, card, home, "sync", "--no-art")}
	runs = append(runs, larrySync(t, card, home, "fetch", "--rom", fmt.Sprint(rom.ID)))
	romPath := runs[1].result("rom_path")
	savePath := filepath.Join(card, "Saves", "GBA", filepath.Base(romPath)+".sav")
	if got, _ := os.ReadFile(savePath); string(got) != newer {
		t.Fatalf("after fetch the device holds %q, want the newest save %q\n%s", got, newer, explain(t, card, home, runs...))
	}

	// The list the in-game menu shows, found by the game's file as it is.
	list := larrySync(t, card, home, "list-saves", "--path", romPath)
	runs = append(runs, list)
	lines := saveLines(list)
	if list.code != 0 || lines[fmt.Sprint(a.ID)] == nil || lines[fmt.Sprint(b.ID)] == nil {
		t.Fatalf("list-saves did not list both saves\n%s", explain(t, card, home, runs...))
	}
	if newest := lines[fmt.Sprint(b.ID)][5]; newest != "1" {
		t.Errorf("save %d is the newest in its slot, listed as %q", b.ID, newest)
	}
	if onDevice := lines[fmt.Sprint(b.ID)][6]; onDevice != "1" {
		t.Errorf("save %d is the one on the device, listed as on-device %q", b.ID, onDevice)
	}

	choose := larrySync(t, card, home, "choose-save", "--path", romPath, "--save", fmt.Sprint(a.ID))
	runs = append(runs, choose)
	if choose.code != 0 {
		t.Fatalf("choose-save failed\n%s", explain(t, card, home, runs...))
	}
	if got, _ := os.ReadFile(savePath); string(got) != older {
		t.Fatalf("after choosing, the device holds %q, want the chosen %q\n%s", got, older, explain(t, card, home, runs...))
	}
	if choose.result("promoted") != "1" {
		t.Errorf("an older save was chosen but not made the newest in its slot\n%s", explain(t, card, home, runs...))
	}

	// What was there is backed up, not lost.
	backups, _ := filepath.Glob(filepath.Join(card, "Saves", "GBA", ".backup", "*"))
	found := false
	for _, backup := range backups {
		if content, _ := os.ReadFile(backup); string(content) == newer {
			found = true
		}
	}
	if !found {
		t.Errorf("the save that was replaced is not in Saves/GBA/.backup (%v)\n%s", backups, explain(t, card, home, runs...))
	}

	// The check before the next play keeps the choice.
	again := larrySync(t, card, home, "fetch", "--rom", fmt.Sprint(rom.ID))
	runs = append(runs, again)
	if got, _ := os.ReadFile(savePath); string(got) != older {
		t.Fatalf("the next fetch replaced the chosen save with %q\n%s", got, explain(t, card, home, runs...))
	}
	if again.result("saves_conflicts") != "0" && again.result("saves_conflicts") != "" {
		t.Errorf("the next fetch saw a conflict over the chosen save\n%s", explain(t, card, home, runs...))
	}

	// And nothing that was on the server is gone.
	after, err := c.GetSaves(rommapi.SaveQuery{RomID: rom.ID})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, save := range after {
		ids[save.ID] = true
	}
	if !ids[a.ID] || !ids[b.ID] {
		t.Errorf("a save was removed from the server while choosing (have %v)", ids)
	}
}
