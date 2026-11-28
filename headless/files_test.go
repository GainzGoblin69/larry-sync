package headless

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileCard(t *testing.T) (string, *FileServer, *httptest.Server) {
	t.Helper()
	card := t.TempDir()
	for path, body := range map[string]string{
		"wifi.txt":               "Home: password\n",
		"Roms/GBA/Game.gba":      "\x00\x01rom",
		"Roms/GBA/Other.gba":     "rom",
		"Saves/GBA/Game.gba.sav": "save",
		".system/version.txt":    "firmware\n",
		".tmp_update/updater":    "boot",
		"Notes/binary.txt":       "a\x00b",
	} {
		full := filepath.Join(card, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server, err := NewFileServer(card, NewEvents(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)
	return card, server, web
}

func request(t *testing.T, method, address, key string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, address, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func at(web *httptest.Server, endpoint, path string) string {
	return web.URL + endpoint + "?path=" + url.QueryEscape(path)
}

func TestFilesNeedTheKey(t *testing.T) {
	_, server, web := fileCard(t)
	if code, _ := request(t, "GET", web.URL+"/", "", ""); code != http.StatusForbidden {
		t.Errorf("page without the key: %d", code)
	}
	if code, body := request(t, "GET", web.URL+"/?k="+server.Key, "", ""); code != 200 || !strings.Contains(body, "Larry Boy") {
		t.Errorf("page with the key: %d", code)
	}
	if code, _ := request(t, "GET", at(web, "/api/list", ""), "wrong", ""); code != http.StatusForbidden {
		t.Errorf("list with a wrong key: %d", code)
	}
	if code, _ := request(t, "POST", at(web, "/api/delete", "wifi.txt"), "", ""); code != http.StatusForbidden {
		t.Errorf("delete without the key: %d", code)
	}
}

func TestFilesListFoldersFirst(t *testing.T) {
	_, server, web := fileCard(t)
	code, body := request(t, "GET", at(web, "/api/list", ""), server.Key, "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var got struct {
		Entries []fileEntry `json:"entries"`
	}
	json.Unmarshal([]byte(body), &got)
	var names []string
	for _, e := range got.Entries {
		names = append(names, e.Name)
		if e.Name == ".system" && !e.Locked {
			t.Error(".system is not marked as the firmware")
		}
		if e.Name == "wifi.txt" && !e.Text {
			t.Error("wifi.txt is not marked as text")
		}
	}
	if strings.Join(names, ",") != ".system,.tmp_update,Notes,Roms,Saves,wifi.txt" {
		t.Errorf("order: %v", names)
	}
}

func TestFilesStayOnTheCard(t *testing.T) {
	card, server, web := fileCard(t)
	outside := filepath.Join(filepath.Dir(card), "outside.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	defer os.Remove(outside)
	for _, path := range []string{"../outside.txt", "Roms/../../outside.txt", "/../outside.txt", `..\outside.txt`} {
		if code, body := request(t, "GET", at(web, "/api/text", path), server.Key, ""); code == 200 && strings.Contains(body, "secret") {
			t.Errorf("read %q outside the card", path)
		}
		request(t, "POST", at(web, "/api/delete", path), server.Key, "")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a file outside the card was deleted")
	}
}

func TestFilesEditText(t *testing.T) {
	card, server, web := fileCard(t)
	code, body := request(t, "GET", at(web, "/api/text", "wifi.txt"), server.Key, "")
	if code != 200 || body != "Home: password\n" {
		t.Fatalf("read: %d %q", code, body)
	}
	if code, body := request(t, "PUT", at(web, "/api/text", "wifi.txt"), server.Key, "Home: new\nCabin: other\n"); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	data, _ := os.ReadFile(filepath.Join(card, "wifi.txt"))
	if string(data) != "Home: new\nCabin: other\n" {
		t.Errorf("saved %q", data)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(card, "*.larry-part")); len(leftovers) != 0 {
		t.Errorf("left %v", leftovers)
	}
	if code, _ := request(t, "GET", at(web, "/api/text", "Notes/binary.txt"), server.Key, ""); code != http.StatusBadRequest {
		t.Errorf("binary opened as text: %d", code)
	}
	if code, _ := request(t, "PUT", at(web, "/api/text", ".system/version.txt"), server.Key, "changed"); code != http.StatusForbidden {
		t.Errorf("firmware file saved: %d", code)
	}
	if data, _ := os.ReadFile(filepath.Join(card, ".system", "version.txt")); string(data) != "firmware\n" {
		t.Error("firmware file changed")
	}
	if code, _ := request(t, "PUT", at(web, "/api/text", "Roms"), server.Key, "x"); code != http.StatusBadRequest {
		t.Errorf("saved over a folder: %d", code)
	}
}

func TestFilesDelete(t *testing.T) {
	card, server, web := fileCard(t)
	if code, body := request(t, "POST", at(web, "/api/delete", "Roms/GBA/Other.gba"), server.Key, ""); code != 200 {
		t.Fatalf("delete file: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(card, "Roms/GBA/Other.gba")); !os.IsNotExist(err) {
		t.Error("file still there")
	}
	if code, _ := request(t, "POST", at(web, "/api/delete", "Saves"), server.Key, ""); code != 200 {
		t.Fatalf("delete folder: %d", code)
	}
	if _, err := os.Stat(filepath.Join(card, "Saves")); !os.IsNotExist(err) {
		t.Error("folder still there")
	}
	for _, path := range []string{"", "/", ".", ".system", ".system/version.txt", ".tmp_update", ".SYSTEM"} {
		if code, _ := request(t, "POST", at(web, "/api/delete", path), server.Key, ""); code == 200 {
			t.Errorf("deleted %q", path)
		}
	}
	if _, err := os.Stat(filepath.Join(card, ".system", "version.txt")); err != nil {
		t.Error("the firmware was deleted")
	}
	if code, _ := request(t, "GET", at(web, "/api/delete", "wifi.txt"), server.Key, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("delete by GET: %d", code)
	}
	if code, _ := request(t, "POST", at(web, "/api/delete", "missing.txt"), server.Key, ""); code != http.StatusNotFound {
		t.Errorf("delete missing: %d", code)
	}
}

func TestFilesDownload(t *testing.T) {
	_, server, web := fileCard(t)
	code, body := request(t, "GET", at(web, "/api/download", "Roms/GBA/Game.gba")+"&k="+server.Key, "", "")
	if code != 200 || body != "\x00\x01rom" {
		t.Errorf("download: %d %q", code, body)
	}
}
