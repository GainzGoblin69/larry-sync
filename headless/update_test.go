package headless

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		commit, tag string
		want        bool
	}{
		{"027f4c0", "build-20261126-larry-boy-advanced-v1-0-027f4c0", false},
		{"027f4c0", "build-20261127-main-0-9a1b2c3", true},
		{"027f4c0a1", "build-20261126-main-0-027f4c0", false},
		{"", "build-20261126-main-0-027f4c0", true},
		{"027f4c0", "", false},
	}
	for _, c := range cases {
		if got := IsNewer(Installed{Commit: c.commit}, Release{Tag: c.tag}); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.commit, c.tag, got, c.want)
		}
	}
}

// firmwareZip is a small zip laid out like MinUI.zip.
func firmwareZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{".system/version.txt", ".tmp_update/updater"} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("x"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGitHub serves one release whose MinUI.zip is body.
func fakeGitHub(t *testing.T, tag string, body []byte, digest string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/me/firmware/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name":     tag,
				"published_at": "2026-11-26T10:00:00Z",
				"assets": []map[string]any{
					{"name": "Firmware-all.zip", "size": 999, "browser_download_url": server.URL + "/all.zip"},
					{"name": UpdateFile, "size": len(body), "browser_download_url": server.URL + "/MinUI.zip", "digest": digest},
				},
			})
		case "/MinUI.zip":
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func cardWithFirmware(t *testing.T, commit string) string {
	t.Helper()
	card := t.TempDir()
	system := filepath.Join(card, ".system")
	os.MkdirAll(system, 0o755)
	os.WriteFile(filepath.Join(system, "version.txt"), []byte("LarryBoyAdvanced-20261125-main-0\n"+commit+"\n"), 0o644)
	os.WriteFile(filepath.Join(system, "larry-repo.txt"), []byte("me/firmware\n"), 0o644)
	return card
}

func TestUpdaterChecksAndDownloads(t *testing.T) {
	firmware := firmwareZip(t)
	sum := sha256.Sum256(firmware)
	server := fakeGitHub(t, "build-20261126-main-0-9a1b2c3", firmware, "sha256:"+hex.EncodeToString(sum[:]))
	card := cardWithFirmware(t, "027f4c0")

	var out bytes.Buffer
	u := NewUpdater(card, NewEvents(&out))
	u.API = server.URL

	if _, _, err := u.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out.String(), "RESULT\tavailable\t1") {
		t.Errorf("a newer build was not offered:\n%s", out.String())
	}

	if err := u.Download(context.Background()); err != nil {
		t.Fatalf("download: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(filepath.Join(card, UpdateFile))
	if err != nil || !bytes.Equal(got, firmware) {
		t.Fatalf("MinUI.zip on the card is not the release's (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(card, UpdateFile+".part")); !os.IsNotExist(err) {
		t.Errorf("the partial file was left behind")
	}
}

func TestUpdaterSameBuildIsNotOffered(t *testing.T) {
	server := fakeGitHub(t, "build-20261126-main-0-027f4c0", firmwareZip(t), "")
	card := cardWithFirmware(t, "027f4c0")
	var out bytes.Buffer
	u := NewUpdater(card, NewEvents(&out))
	u.API = server.URL
	if _, _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "RESULT\tavailable\t0") {
		t.Errorf("the installed build was offered as an update:\n%s", out.String())
	}
}

func TestUpdaterRefusesADamagedDownload(t *testing.T) {
	firmware := firmwareZip(t)
	server := fakeGitHub(t, "build-20261126-main-0-9a1b2c3", firmware, "sha256:"+strings.Repeat("0", 64))
	card := cardWithFirmware(t, "027f4c0")
	u := NewUpdater(card, NewEvents(&bytes.Buffer{}))
	u.API = server.URL
	if err := u.Download(context.Background()); err == nil {
		t.Fatal("a download that did not match its digest was accepted")
	}
	if _, err := os.Stat(filepath.Join(card, UpdateFile)); !os.IsNotExist(err) {
		t.Error("a damaged update was put where the boot script would install it")
	}

	notZip := fakeGitHub(t, "build-20261126-main-0-9a1b2c3", []byte("not a zip"), "")
	u.API = notZip.URL
	if err := u.Download(context.Background()); err == nil {
		t.Fatal("a file that is not a zip was accepted")
	}
	if _, err := os.Stat(filepath.Join(card, UpdateFile)); !os.IsNotExist(err) {
		t.Error("a broken update was put where the boot script would install it")
	}
}

func TestUpdaterNeedsASource(t *testing.T) {
	card := t.TempDir()
	u := NewUpdater(card, NewEvents(&bytes.Buffer{}))
	if _, _, err := u.Check(context.Background()); err == nil {
		t.Fatal("checked with no repository named")
	}
}
