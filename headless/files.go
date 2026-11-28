package headless

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// File Access: a web page, served from the device to a phone on the same
// Wi-Fi, for looking through the card, deleting files and editing text files.
// The device shows the page's address as a QR code. The address carries a
// key made up each time; without it the server answers nothing. It runs
// until the device's screen for it is closed, which ends the process.

// FilesPort is the port tried first; another is used if it is taken.
const FilesPort = 8080

// maxTextSize is the largest file the page will open for editing.
const maxTextSize = 2 << 20

// textExtensions are opened as text without looking inside.
var textExtensions = map[string]bool{
	".txt": true, ".cfg": true, ".ini": true, ".json": true, ".md": true,
	".log": true, ".xml": true, ".conf": true, ".sh": true, ".csv": true,
	".yaml": true, ".yml": true, ".m3u": true, ".cue": true, ".toml": true,
}

// firmwareFolders hold the firmware itself: they can be looked through, but
// nothing in them can be deleted or changed, so the device always starts.
var firmwareFolders = []string{".system", ".tmp_update"}

// FileServer serves the card at Root to whoever has Key.
type FileServer struct {
	Root   string
	Key    string
	Events *Events
}

// NewFileServer returns a server for the card at root with a fresh key.
func NewFileServer(root string, events *Events) (*FileServer, error) {
	key := make([]byte, 12)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &FileServer{Root: abs, Key: hex.EncodeToString(key), Events: events}, nil
}

// Serve listens on the device's Wi-Fi address and reports the page's
// address (RESULT url), then serves until ctx ends.
func (f *FileServer) Serve(ctx context.Context) error {
	ip, err := localIP()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", FilesPort))
	if err != nil {
		listener, err = net.Listen("tcp", ":0")
		if err != nil {
			return fmt.Errorf("cannot open File Access: %w", err)
		}
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{Handler: f.Handler(), ReadHeaderTimeout: 10 * time.Second}
	f.Events.Result("url", fmt.Sprintf("http://%s:%d/?k=%s", ip, port, f.Key))
	f.Events.Result("address", fmt.Sprintf("%s:%d", ip, port))

	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// localIP is the device's address on the local network, preferring Wi-Fi.
func localIP() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var found string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if strings.HasPrefix(iface.Name, "wlan") {
				return ipnet.IP.String(), nil
			}
			if found == "" {
				found = ipnet.IP.String()
			}
		}
	}
	if found == "" {
		return "", errors.New("not connected to a network: connect to Wi-Fi first")
	}
	return found, nil
}

// Handler is the page and what it asks for.
func (f *FileServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.page)
	mux.HandleFunc("/api/list", f.api(f.list))
	mux.HandleFunc("/api/text", f.api(f.text))
	mux.HandleFunc("/api/delete", f.api(f.delete))
	mux.HandleFunc("/api/download", f.api(f.download))
	return mux
}

func (f *FileServer) allowed(r *http.Request) bool {
	given := r.Header.Get("X-Key")
	if given == "" {
		given = r.URL.Query().Get("k")
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(f.Key)) == 1
}

func (f *FileServer) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !f.allowed(r) {
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width"><body style="font-family:sans-serif;background:#000;color:#fff;padding:2em">Scan the code on the Hammer to open its files.</body>`)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, filesPage)
}

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string { return e.message }

func badRequest(message string) error { return apiError{http.StatusBadRequest, message} }

func (f *FileServer) api(handle func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !f.allowed(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "This page's key has expired. Scan the code on the Hammer again."})
			return
		}
		if err := handle(w, r); err != nil {
			status := http.StatusInternalServerError
			var known apiError
			if errors.As(err, &known) {
				status = known.status
			} else if errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
				err = errors.New("that is no longer on the card")
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// resolve turns a path on the card, as the page names it ("Roms/GBA"), into
// one on the device, refusing any that would leave the card.
func (f *FileServer) resolve(rel string) (string, string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	if strings.Contains(clean, "\x00") {
		return "", "", badRequest("that name is not allowed")
	}
	full := filepath.Join(f.Root, filepath.FromSlash(clean))
	inside, err := filepath.Rel(f.Root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", "", badRequest("that is outside the card")
	}
	return full, strings.TrimPrefix(clean, "/"), nil
}

// isFirmware reports whether the card path is the firmware or inside it.
func isFirmware(rel string) bool {
	top := strings.SplitN(rel, "/", 2)[0]
	for _, folder := range firmwareFolders {
		if strings.EqualFold(top, folder) {
			return true
		}
	}
	return false
}

type fileEntry struct {
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	Text     bool   `json:"text"`
	Locked   bool   `json:"locked"`
}

func (f *FileServer) list(w http.ResponseWriter, r *http.Request) error {
	full, rel, err := f.resolve(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	items, err := os.ReadDir(full)
	if err != nil {
		return err
	}
	entries := []fileEntry{}
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			continue
		}
		child := path.Join(rel, item.Name())
		entry := fileEntry{
			Name:     item.Name(),
			Dir:      item.IsDir(),
			Modified: info.ModTime().Unix(),
			Locked:   isFirmware(child),
		}
		if !entry.Dir {
			entry.Size = info.Size()
			entry.Text = info.Size() <= maxTextSize && textExtensions[strings.ToLower(filepath.Ext(item.Name()))]
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "locked": isFirmware(rel), "entries": entries})
	return nil
}

// text sends a text file (GET) or saves it (PUT).
func (f *FileServer) text(w http.ResponseWriter, r *http.Request) error {
	full, rel, err := f.resolve(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(full)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return badRequest("that is a folder")
		}
		if info.Size() > maxTextSize {
			return badRequest("that file is too large to edit here")
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			return badRequest("that file is not text")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)
		return nil

	case http.MethodPut:
		if rel == "" || isFirmware(rel) {
			return apiError{http.StatusForbidden, "the firmware's own files cannot be changed"}
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxTextSize+1))
		if err != nil {
			return err
		}
		if len(data) > maxTextSize {
			return badRequest("that is too much text to save here")
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(full); err == nil {
			if info.IsDir() {
				return badRequest("that is a folder")
			}
			mode = info.Mode().Perm()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// written beside it, then put in its place, so a cut-off save never
		// leaves half a file
		partial := full + ".larry-part"
		if err := os.WriteFile(partial, data, mode); err != nil {
			return err
		}
		if err := os.Rename(partial, full); err != nil {
			os.Remove(partial)
			return err
		}
		f.Events.Info("Saved %s", rel)
		writeJSON(w, http.StatusOK, map[string]any{"saved": rel, "size": len(data)})
		return nil
	}
	return apiError{http.StatusMethodNotAllowed, "not allowed"}
}

func (f *FileServer) delete(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return apiError{http.StatusMethodNotAllowed, "not allowed"}
	}
	full, rel, err := f.resolve(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	if rel == "" {
		return apiError{http.StatusForbidden, "the whole card cannot be deleted"}
	}
	if isFirmware(rel) {
		return apiError{http.StatusForbidden, "the firmware's own files cannot be deleted"}
	}
	if _, err := os.Lstat(full); err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil {
		return err
	}
	f.Events.Info("Deleted %s", rel)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": rel})
	return nil
}

func (f *FileServer) download(w http.ResponseWriter, r *http.Request) error {
	full, _, err := f.resolve(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	file, err := os.Open(full)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return badRequest("that is a folder")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return nil
}
