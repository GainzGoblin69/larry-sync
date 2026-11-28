package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCopyparty serves folders the way Copyparty does: ?ls lists a folder as
// JSON, anything else is a file, and every request needs the password.
type fakeCopyparty struct {
	password string
	tree     map[string][]string // folder path -> entries; a name ending in / is a folder
	lists    int
}

func (f *fakeCopyparty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("PW") != f.password {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>login</html>"))
		return
	}
	if _, ok := r.URL.Query()["ls"]; ok {
		f.lists++
		entries, ok := f.tree[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		type entry struct {
			Href string `json:"href"`
		}
		body := struct {
			Dirs  []entry `json:"dirs"`
			Files []entry `json:"files"`
		}{[]entry{}, []entry{}}
		for _, name := range entries {
			escaped := strings.ReplaceAll(name, " ", "%20")
			if strings.HasSuffix(name, "/") {
				body.Dirs = append(body.Dirs, entry{escaped})
			} else {
				body.Files = append(body.Files, entry{escaped})
			}
		}
		json.NewEncoder(w).Encode(body)
		return
	}
	w.Write([]byte("song:" + r.URL.Path))
}

func musicCard(t *testing.T, contents string) string {
	t.Helper()
	card := t.TempDir()
	if err := os.WriteFile(filepath.Join(card, MusicFile), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return card
}

func results(out string) map[string]string {
	r := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 && f[0] == "RESULT" {
			r[f[1]] = f[2]
		}
	}
	return r
}

func TestReadMusicFile(t *testing.T) {
	card := musicCard(t, `# comment
Game Music: https://music.example/music
game music password: "pa:ss"
johnny music password: other
`)
	sources, err := ReadMusicFile(filepath.Join(card, MusicFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := sources[MusicGame]; got.URL != "https://music.example/music/" || got.Password != "pa:ss" {
		t.Errorf("game music = %+v", got)
	}
	if got := sources[MusicJohnny]; got.URL != DefaultJohnnyURL || got.Password != "other" {
		t.Errorf("johnny music = %+v", got)
	}

	card = musicCard(t, "game music: https://music.example/music/\ngame music password: x\njohnny music password:\n")
	sources, _ = ReadMusicFile(filepath.Join(card, MusicFile))
	if _, ok := sources[MusicJohnny]; ok {
		t.Error("Johnny Music was set up with no password")
	}
}

func TestSongTitle(t *testing.T) {
	cases := map[string]string{
		"01 - Main Theme.mp3":            "Main Theme",
		"1-02 Stickerbush_Symphony.flac": "Stickerbush Symphony",
		"Aquatic Ambience.ogg":           "Aquatic Ambience",
		"07.mp3":                         "07",
	}
	for in, want := range cases {
		if got := SongTitle(in); got != want {
			t.Errorf("SongTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGameMusicPlaysEverySongOnceBeforeRepeating(t *testing.T) {
	fake := &fakeCopyparty{password: "pw", tree: map[string][]string{
		"/music/":              {"DKC3 OST/", "Zelda/", "cover.jpg"},
		"/music/DKC3 OST/":     {"01 - Main Theme.mp3", "02 - Water World.flac", "cover.jpg", "notes.txt"},
		"/music/Zelda/":        {"Disc 1/", "Overworld.ogg"},
		"/music/Zelda/Disc 1/": {"Title.wav", "Ballad.m4a"},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	card := musicCard(t, "game music: "+server.URL+"/music/\ngame music password: pw\n")
	dir := t.TempDir()

	seen := map[string]int{}
	for round := 0; round < 2; round++ {
		for i := 0; i < 4; i++ {
			var out bytes.Buffer
			m := NewMusic(card, dir, NewEvents(&out))
			if err := m.Next(context.Background(), MusicGame); err != nil {
				t.Fatalf("next: %v", err)
			}
			r := results(out.String())
			data, err := os.ReadFile(r["path"])
			if err != nil {
				t.Fatalf("no song at %q: %v", r["path"], err)
			}
			seen[string(data)]++
			if round == 0 && seen[string(data)] > 1 {
				t.Fatalf("%s repeated before every song played", data)
			}
			if r["title"] == "" {
				t.Errorf("no title for %s", data)
			}
		}
	}
	if len(seen) != 4 {
		t.Errorf("played %d different songs, want the 4 playable ones: %v", len(seen), seen)
	}
	for song, n := range seen {
		if n != 2 {
			t.Errorf("%s played %d times in two rounds", song, n)
		}
	}
	// the folder is listed once, not for every song
	if fake.lists > 5 {
		t.Errorf("listed folders %d times for 8 songs", fake.lists)
	}
	// only the last few songs are kept on the card
	songs, _ := os.ReadDir(filepath.Join(dir, "songs"))
	if len(songs) > musicKeep {
		t.Errorf("%d songs left on the card, want at most %d", len(songs), musicKeep)
	}
}

func TestJohnnyMusicWalksToARandomSong(t *testing.T) {
	fake := &fakeCopyparty{password: "jp", tree: map[string][]string{
		"/johnny/":                  {"Artist A/", "Empty/", "Artist B/"},
		"/johnny/Artist A/":         {"Album 1/"},
		"/johnny/Artist A/Album 1/": {"01 Song.mp3", "02 Song.mp3"},
		"/johnny/Empty/":            {},
		"/johnny/Artist B/":         {"Single.flac", "art.png"},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	card := musicCard(t, "johnny music: "+server.URL+"/johnny\njohnny music password: jp\n")

	got := map[string]bool{}
	for i := 0; i < 20; i++ {
		var out bytes.Buffer
		m := NewMusic(card, t.TempDir(), NewEvents(&out))
		m.Rand = rand.New(rand.NewSource(int64(i)))
		if err := m.Next(context.Background(), MusicJohnny); err != nil {
			t.Fatalf("next: %v", err)
		}
		data, _ := os.ReadFile(results(out.String())["path"])
		got[string(data)] = true
	}
	if len(got) < 2 {
		t.Errorf("20 picks found only %v", got)
	}
	for song := range got {
		if !strings.HasSuffix(song, ".mp3") && !strings.HasSuffix(song, ".flac") {
			t.Errorf("picked something that is not a song: %s", song)
		}
	}
}

func TestMusicWithAWrongPasswordSaysSo(t *testing.T) {
	server := httptest.NewServer(&fakeCopyparty{password: "right", tree: map[string][]string{"/music/": {"a.mp3"}}})
	defer server.Close()
	card := musicCard(t, "game music: "+server.URL+"/music/\ngame music password: wrong\n")
	m := NewMusic(card, t.TempDir(), NewEvents(&bytes.Buffer{}))
	err := m.Next(context.Background(), MusicGame)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("got %v, want a refused password", err)
	}
	if err := m.Next(context.Background(), MusicJohnny); err != ErrMusicNotSetUp {
		t.Errorf("Johnny Music with none set up: got %v", err)
	}
}
