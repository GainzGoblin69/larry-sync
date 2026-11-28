package headless

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grout/romm"
	"grout/saves"
	"grout/settings"
)

func TestEventsWriteOneTabSeparatedLineEach(t *testing.T) {
	var out bytes.Buffer
	events := NewEvents(&out)

	events.Stage("rom", "Chrono\tTrigger\n(USA)")
	events.Result("rom_path", "/mnt/SDCARD/Roms/SNES/Chrono Trigger.sfc")
	events.Info("%d cover(s) could not be downloaded", 2)

	want := "STAGE\trom\tChrono Trigger (USA)\n" +
		"RESULT\trom_path\t/mnt/SDCARD/Roms/SNES/Chrono Trigger.sfc\n" +
		"INFO\t2 cover(s) could not be downloaded\n"
	if got := out.String(); got != want {
		t.Errorf("events =\n%q\nwant\n%q", got, want)
	}
}

func TestEventsProgressSkipsRepeatedPercentages(t *testing.T) {
	var out bytes.Buffer
	events := NewEvents(&out)

	events.Progress(10, 1000)
	events.Progress(15, 1000) // still 1%
	events.Progress(500, 1000)
	events.Progress(2000, 1000) // never past 100%

	want := "PROGRESS\t1\t10\t1000\n" +
		"PROGRESS\t50\t500\t1000\n" +
		"PROGRESS\t100\t2000\t1000\n"
	if got := out.String(); got != want {
		t.Errorf("progress =\n%q\nwant\n%q", got, want)
	}
}

func TestEventsStageResetsProgress(t *testing.T) {
	var out bytes.Buffer
	events := NewEvents(&out)

	events.Progress(100, 100)
	events.Stage("extract", "")
	events.Progress(100, 100)

	if got := strings.Count(out.String(), "PROGRESS\t100"); got != 2 {
		t.Errorf("got %d progress lines at 100%%, want one per stage (2):\n%s", got, out.String())
	}
}

func TestTransferFetchWritesFileAndSendsToken(t *testing.T) {
	const body = "not really a rom"
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Write([]byte(body))
	}))
	defer server.Close()

	host := settings.Host{RootURI: server.URL, Token: "secret"}
	dest := filepath.Join(t.TempDir(), "Game Boy Advance (GBA)", "Game (USA).gba")

	var lastDone, lastTotal int64
	err := newTransfer(host, time.Minute).fetch(context.Background(),
		server.URL+"/api/roms/7/content/Game (USA).gba", dest, 0,
		func(done, total int64) { lastDone, lastTotal = done, total })
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading the downloaded file: %v", err)
	}
	if string(data) != body {
		t.Errorf("file holds %q, want %q", data, body)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the host's bearer token", gotAuth)
	}
	if gotPath != "/api/roms/7/content/Game (USA).gba" {
		t.Errorf("requested path %q, want the name with its space intact", gotPath)
	}
	if lastDone != int64(len(body)) || lastTotal != int64(len(body)) {
		t.Errorf("last progress = %d of %d, want %d of %d", lastDone, lastTotal, len(body), len(body))
	}
	if _, err := os.Stat(dest + partSuffix); !os.IsNotExist(err) {
		t.Errorf("the partial file should be gone after a successful download")
	}
}

func TestTransferFetchLeavesNothingBehindOnFailure(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", http.StatusNotFound)
			},
		},
		{
			name: "body shorter than promised",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1000")
				w.Write([]byte("short"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()

			dir := t.TempDir()
			dest := filepath.Join(dir, "game.gba")

			err := newTransfer(settings.Host{RootURI: server.URL}, time.Minute).
				fetch(context.Background(), server.URL+"/game.gba", dest, 0, nil)
			if err == nil {
				t.Fatal("fetch succeeded, want an error")
			}

			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Errorf("a failed download left %d file(s) behind, want none", len(entries))
			}
		})
	}
}

func TestStateRoundTripsAndForgetsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	rom := filepath.Join(dir, "game.gba")
	if err := os.WriteFile(rom, []byte("rom"), 0644); err != nil {
		t.Fatal(err)
	}

	state := State{PlatformsBinding: map[string]string{"megadrive": "genesis"}}
	state.RecordDownload(42, rom, 3)
	state.RecordDownload(43, filepath.Join(dir, "gone.gba"), 3)
	if err := state.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadState(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := loaded.PlatformsBinding["megadrive"]; got != "genesis" {
		t.Errorf("binding = %q, want genesis", got)
	}
	if got := loaded.DownloadedPath(42); got != rom {
		t.Errorf("DownloadedPath(42) = %q, want %q", got, rom)
	}
	if got := loaded.DownloadedPath(43); got != "" {
		t.Errorf("DownloadedPath(43) = %q, want empty for a file that is not there", got)
	}
	if got := loaded.DownloadedPath(44); got != "" {
		t.Errorf("DownloadedPath(44) = %q, want empty for a game never fetched", got)
	}
}

func TestLoadStateTreatsMissingFileAsEmpty(t *testing.T) {
	state, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Downloads) != 0 || len(state.PlatformsBinding) != 0 {
		t.Errorf("state = %+v, want empty", state)
	}
}

func TestSelectSaveItems(t *testing.T) {
	items := []saves.SyncItem{
		{LocalSave: saves.LocalSave{RomID: 1}, Action: saves.ActionUpload},
		{LocalSave: saves.LocalSave{RomID: 1}, Action: saves.ActionDownload},
		{LocalSave: saves.LocalSave{RomID: 1}, Action: saves.ActionConflict},
		{LocalSave: saves.LocalSave{RomID: 2}, Action: saves.ActionUpload},
		// A save that exists only on the server has no local side.
		{RemoteSave: &romm.Save{RomID: 3}, Action: saves.ActionDownload},
	}

	tests := []struct {
		name    string
		options SaveOptions
		want    []saves.SyncAction
	}{
		{
			name:    "everything",
			options: SaveOptions{Direction: DirectionBoth},
			want: []saves.SyncAction{saves.ActionUpload, saves.ActionDownload, saves.ActionConflict,
				saves.ActionUpload, saves.ActionDownload},
		},
		{
			name:    "pull for one game keeps its conflict",
			options: SaveOptions{RomID: 1, Direction: DirectionPull},
			want:    []saves.SyncAction{saves.ActionDownload, saves.ActionConflict},
		},
		{
			name:    "push for one game",
			options: SaveOptions{RomID: 1, Direction: DirectionPush},
			want:    []saves.SyncAction{saves.ActionUpload, saves.ActionConflict},
		},
		{
			name:    "server-only save is found by its remote rom id",
			options: SaveOptions{RomID: 3, Direction: DirectionPull},
			want:    []saves.SyncAction{saves.ActionDownload},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectSaveItems(items, test.options)
			if len(got) != len(test.want) {
				t.Fatalf("kept %d item(s), want %d", len(got), len(test.want))
			}
			for i, item := range got {
				if item.Action != test.want[i] {
					t.Errorf("item %d is %s, want %s", i, item.Action, test.want[i])
				}
			}
		})
	}
}

func TestParseDirection(t *testing.T) {
	for input, want := range map[string]Direction{
		"": DirectionBoth, "both": DirectionBoth, "pull": DirectionPull, "push": DirectionPush,
	} {
		got, err := ParseDirection(input)
		if err != nil || got != want {
			t.Errorf("ParseDirection(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseDirection("sideways"); err == nil {
		t.Error("ParseDirection(\"sideways\") succeeded, want an error")
	}
}

func TestIsInside(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "mnt", "SDCARD", "Roms", "GBA")

	tests := []struct {
		child string
		want  bool
	}{
		{filepath.Join(root, "Game.gba"), true},
		{filepath.Join(root, "Game", "Game (Disc 1).chd"), true},
		{root, false},
		{filepath.Join(root, ".."), false},
		{filepath.Join(root, "..", "SNES", "Other.sfc"), false},
		{filepath.Join(root, "..", "GBA-other", "Game.gba"), false},
		{"", false},
	}
	for _, test := range tests {
		if got := isInside(root, test.child); got != test.want {
			t.Errorf("isInside(%q, %q) = %v, want %v", root, test.child, got, test.want)
		}
	}
}

func TestExportFileName(t *testing.T) {
	for input, want := range map[string]string{
		"gba":          "gba.tsv",
		"sega/genesis": "sega_genesis.tsv",
		"..":           "_.tsv",
		"":             "_.tsv",
	} {
		if got := exportFileName(input); got != want {
			t.Errorf("exportFileName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDisplayNameFallsBackToTheFile(t *testing.T) {
	tests := []struct {
		game romm.Rom
		want string
	}{
		{romm.Rom{Name: "Metroid Fusion", FsNameNoExt: "Metroid Fusion (USA)"}, "Metroid Fusion"},
		{romm.Rom{Name: "  ", FsNameNoExt: "Metroid Fusion (USA)"}, "Metroid Fusion (USA)"},
		{romm.Rom{FsName: "Metroid Fusion (USA).gba"}, "Metroid Fusion (USA).gba"},
	}
	for _, test := range tests {
		if got := displayName(test.game); got != test.want {
			t.Errorf("displayName(%+v) = %q, want %q", test.game, got, test.want)
		}
	}
}
