package headless

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Menu music streams from a Copyparty server: one folder of game soundtracks,
// and optionally Johnny Music, a second folder with its own password. Both
// are set in music.txt at the top of the card:
//
//	game music: https://music.example/music/
//	game music password: secret
//	johnny music password: other-secret
//	johnny music: https://music.example/johnny/   (optional; this is the default)
//
// Each "music next" brings down one song for the player to play, into the
// music folder beside the catalog, and says where it is.

// MusicFile is the settings file at the top of the card.
const MusicFile = "music.txt"

// DefaultJohnnyURL is where Johnny Music is when music.txt names no folder.
const DefaultJohnnyURL = "https://music.alice99.box.ca/johnny/"

// MusicSources are the two kinds of music.
const (
	MusicGame   = "game"
	MusicJohnny = "johnny"
)

// ErrMusicNotSetUp means music.txt does not set up the music asked for.
var ErrMusicNotSetUp = errors.New("that music is not set up in " + MusicFile)

// musicAudio are the kinds of file the player can play.
var musicAudio = map[string]bool{".mp3": true, ".flac": true, ".ogg": true, ".wav": true}

// MusicSource is one folder of music and its password.
type MusicSource struct {
	URL      string
	Password string
}

// ReadMusicFile reads music.txt: "key: value" lines, # for comments, as the
// card's other settings files are written.
func ReadMusicFile(path string) (map[string]MusicSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		values[strings.ToLower(strings.Join(strings.Fields(key), " "))] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sources := map[string]MusicSource{}
	if values["game music"] != "" {
		sources[MusicGame] = MusicSource{URL: folderURL(values["game music"]), Password: values["game music password"]}
	}
	if values["johnny music password"] != "" {
		address := values["johnny music"]
		if address == "" {
			address = DefaultJohnnyURL
		}
		sources[MusicJohnny] = MusicSource{URL: folderURL(address), Password: values["johnny music password"]}
	}
	return sources, nil
}

func folderURL(address string) string {
	address = strings.TrimSpace(address)
	if !strings.HasSuffix(address, "/") {
		address += "/"
	}
	return address
}

// Song is one song, by its address on the server.
type Song struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Album string `json:"album"`
}

// Music fetches songs for the player.
type Music struct {
	Card   string // the top of the SD card, where music.txt is
	Dir    string // where songs and the shuffle are kept
	Events *Events
	Client *http.Client
	Rand   *rand.Rand
}

// NewMusic returns a Music keeping its files in dir.
func NewMusic(card, dir string, events *Events) *Music {
	return &Music{
		Card:   card,
		Dir:    dir,
		Events: events,
		Client: &http.Client{Timeout: 30 * time.Second},
		Rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Next brings down the next song from source and reports it:
// RESULT path, title, album.
//
// Game music plays every song once, in a shuffled order, before any repeats;
// the folder is listed whole and the order kept between songs. Johnny Music
// is a folder too large to list, so each song is found by stepping from the
// top into random folders until a song turns up.
func (m *Music) Next(ctx context.Context, sourceName string) error {
	sources, err := ReadMusicFile(filepath.Join(m.Card, MusicFile))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMusicNotSetUp, err)
	}
	source, ok := sources[sourceName]
	if !ok {
		return ErrMusicNotSetUp
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}

	var song Song
	switch sourceName {
	case MusicGame:
		song, err = m.nextShuffled(ctx, source)
	default:
		song, err = m.randomSong(ctx, source)
	}
	if err != nil {
		return err
	}

	local, err := m.download(ctx, source, song)
	if err != nil {
		return err
	}
	m.Events.Result("path", local)
	m.Events.Result("title", song.Title)
	m.Events.Result("album", song.Album)
	return nil
}

// listing is one folder: its sub-folders and songs, as addresses.
type listing struct {
	dirs  []string
	songs []string
}

// list reads one folder through Copyparty's ?ls.
func (m *Music) list(ctx context.Context, source MusicSource, dir string) (listing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dir+"?ls", nil)
	if err != nil {
		return listing{}, err
	}
	req.Header.Set("PW", source.Password)
	resp, err := m.Client.Do(req)
	if err != nil {
		return listing{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return listing{}, errors.New("the music password was refused")
	case resp.StatusCode == http.StatusNotFound:
		return listing{}, errors.New("that music folder was not found on the server")
	case resp.StatusCode >= 400:
		return listing{}, fmt.Errorf("the music server answered %s", resp.Status)
	}
	var body struct {
		Dirs  []struct{ Href string } `json:"dirs"`
		Files []struct{ Href string } `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		// Copyparty answers a wrong password with its login page
		return listing{}, errors.New("the music password was refused")
	}
	var out listing
	for _, d := range body.Dirs {
		href := strings.SplitN(d.Href, "?", 2)[0]
		if href == "" || strings.HasPrefix(href, ".") {
			continue
		}
		if !strings.HasSuffix(href, "/") {
			href += "/"
		}
		out.dirs = append(out.dirs, dir+href)
	}
	for _, f := range body.Files {
		href := strings.SplitN(f.Href, "?", 2)[0]
		if musicAudio[strings.ToLower(path.Ext(href))] {
			out.songs = append(out.songs, dir+href)
		}
	}
	return out, nil
}

// songFrom names a song by its address: the file's name without its number
// and extension, and the folder it is in.
func songFrom(source MusicSource, address string) Song {
	name := address[strings.LastIndex(address, "/")+1:]
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	album := ""
	rest := strings.TrimPrefix(address, source.URL)
	if i := strings.Index(rest, "/"); i > 0 {
		album = rest[:i]
		if unescaped, err := url.PathUnescape(album); err == nil {
			album = unescaped
		}
	}
	return Song{URL: address, Title: SongTitle(name), Album: album}
}

var trackNumber = regexp.MustCompile(`^\s*(\d{1,3}[-.]?)?\d{1,3}\s*[-._)]*\s*`)

// SongTitle is "Main Theme" for "01 - Main Theme.mp3".
func SongTitle(fileName string) string {
	base := strings.TrimSuffix(fileName, path.Ext(fileName))
	title := strings.TrimSpace(strings.ReplaceAll(trackNumber.ReplaceAllString(base, ""), "_", " "))
	if title == "" {
		return base
	}
	return title
}

// randomSong steps from the top of source into random folders until it
// lands on a song. Folders are weighed by count, not size, so songs in small
// folders come up more often than in large ones; listing the whole folder
// would take too long.
func (m *Music) randomSong(ctx context.Context, source MusicSource) (Song, error) {
	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		dir := source.URL
		for depth := 0; depth < 12; depth++ {
			found, err := m.list(ctx, source, dir)
			if err != nil {
				if attempt == 0 && depth == 0 {
					return Song{}, err
				}
				lastErr = err
				break
			}
			total := len(found.dirs) + len(found.songs)
			if total == 0 {
				break
			}
			pick := m.Rand.Intn(total)
			if pick >= len(found.dirs) {
				return songFrom(source, found.songs[pick-len(found.dirs)]), nil
			}
			dir = found.dirs[pick]
		}
	}
	if lastErr != nil {
		return Song{}, lastErr
	}
	return Song{}, errors.New("no songs turned up in that music folder")
}

// gameList is the whole game music folder, kept between songs.
type gameList struct {
	URL     string    `json:"url"`
	Listed  time.Time `json:"listed"`
	Songs   []Song    `json:"songs"`
	Pending []int     `json:"pending"` // songs not yet played this time round, in order
	Last    string    `json:"last"`
}

// gameListAge is how long the game music list is trusted before the folder
// is read again, picking up music added since.
const gameListAge = 6 * time.Hour

func (m *Music) nextShuffled(ctx context.Context, source MusicSource) (Song, error) {
	statePath := filepath.Join(m.Dir, "game-music.json")
	var state gameList
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}

	if state.URL != source.URL || len(state.Songs) == 0 || time.Since(state.Listed) > gameListAge {
		songs, err := m.walk(ctx, source, source.URL, 0)
		if err != nil {
			if len(state.Songs) == 0 || state.URL != source.URL {
				return Song{}, err
			}
			// keep playing what was listed before
		} else {
			if len(songs) == 0 {
				return Song{}, errors.New("no songs turned up in that music folder")
			}
			// the round in progress carries on with the songs still there
			left := map[string]bool{}
			for _, i := range state.Pending {
				if i >= 0 && i < len(state.Songs) {
					left[state.Songs[i].URL] = true
				}
			}
			state = gameList{URL: source.URL, Listed: time.Now(), Songs: songs, Last: state.Last}
			for i, song := range songs {
				if left[song.URL] {
					state.Pending = append(state.Pending, i)
				}
			}
		}
	}

	if len(state.Pending) == 0 {
		state.Pending = m.Rand.Perm(len(state.Songs))
		// a new round does not start with the song that ended the last one
		if len(state.Pending) > 1 && state.Songs[state.Pending[0]].URL == state.Last {
			state.Pending[0], state.Pending[1] = state.Pending[1], state.Pending[0]
		}
	}
	song := state.Songs[state.Pending[0]]
	state.Pending = state.Pending[1:]
	state.Last = song.URL

	if data, err := json.Marshal(state); err == nil {
		partial := statePath + ".part"
		if os.WriteFile(partial, data, 0o644) == nil {
			_ = os.Rename(partial, statePath)
		}
	}
	return song, nil
}

// walk lists every song under dir, a few folders deep.
func (m *Music) walk(ctx context.Context, source MusicSource, dir string, depth int) ([]Song, error) {
	found, err := m.list(ctx, source, dir)
	if err != nil {
		return nil, err
	}
	var songs []Song
	for _, address := range found.songs {
		songs = append(songs, songFrom(source, address))
	}
	if depth >= 4 {
		return songs, nil
	}
	for _, sub := range found.dirs {
		more, err := m.walk(ctx, source, sub, depth+1)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // one folder that cannot be read does not lose the rest
		}
		songs = append(songs, more...)
	}
	return songs, nil
}

// musicKeep is how many songs are kept on the card: the one playing, the
// three before it that the player can go back to (Select + left), and the
// one after it.
const musicKeep = 5

// download brings the song down to the music folder, named in order so the
// oldest can be cleared away.
func (m *Music) download(ctx context.Context, source MusicSource, song Song) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, song.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PW", source.Password)
	client := *m.Client
	client.Timeout = 0 // a long song on a slow connection; ctx still stops it
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the music server answered %s for %s", resp.Status, song.Title)
	}

	ext := strings.ToLower(path.Ext(strings.SplitN(song.URL, "?", 2)[0]))
	songs := filepath.Join(m.Dir, "songs")
	if err := os.MkdirAll(songs, 0o755); err != nil {
		return "", err
	}
	local := filepath.Join(songs, fmt.Sprintf("%d%s", time.Now().UnixNano(), ext))
	partial := local + ".part"
	file, err := os.Create(partial)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(file, resp.Body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(partial)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("bringing down %s: %w", song.Title, err)
	}
	if err := os.Rename(partial, local); err != nil {
		os.Remove(partial)
		return "", err
	}
	clearOldSongs(songs, musicKeep)
	return local, nil
}

// clearOldSongs leaves the newest keep songs in dir, and no partial ones
// left by a download that was stopped.
func clearOldSongs(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var whole []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".part") {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		whole = append(whole, name)
	}
	// names are the time they came down, so in name order they are oldest first
	if len(whole) <= keep {
		return
	}
	sort.Strings(whole)
	for _, name := range whole[:len(whole)-keep] {
		os.Remove(filepath.Join(dir, name))
	}
}
