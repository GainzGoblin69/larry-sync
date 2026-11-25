package archive

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/atomic"
)

func writeZip(t *testing.T, path string, names ...string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	w := zip.NewWriter(file)
	for _, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnzip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "Game.zip")
	writeZip(t, zipPath, "Game.sfc", "extras/readme.txt")

	dest := filepath.Join(dir, "roms")
	if err := Unzip(zipPath, dest, &atomic.Float64{}); err != nil {
		t.Fatalf("Unzip: %v", err)
	}
	for _, name := range []string{"Game.sfc", "extras/readme.txt"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s was not unpacked: %v", name, err)
		}
	}
}

// Archives come from the server. One naming a file outside the rom folder
// must not get to write it.
func TestUnzip_RefusesEntriesOutsideTheDestination(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "sub/../../escaped.txt", "/escaped-absolute.txt"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			zipPath := filepath.Join(dir, "Evil.zip")
			writeZip(t, zipPath, name)

			dest := filepath.Join(dir, "roms")
			err := Unzip(zipPath, dest, &atomic.Float64{})

			escaped := filepath.Join(dir, "escaped.txt")
			if _, statErr := os.Stat(escaped); statErr == nil {
				t.Fatalf("%q was written outside the destination", name)
			}
			// Go's own zip reader may refuse such a name before the guard here
			// sees it (zip.ErrInsecurePath); either refusal will do.
			refused := errors.Is(err, ErrOutsideDestination) || errors.Is(err, zip.ErrInsecurePath)
			// An absolute name is joined under the destination, which is
			// safe; a name that climbs out must be refused.
			if filepath.IsAbs(name) {
				if err != nil && !refused {
					t.Fatalf("Unzip(%q) = %v, want it kept inside the destination", name, err)
				}
				return
			}
			if !refused {
				t.Fatalf("Unzip(%q) = %v, want it refused", name, err)
			}
		})
	}
}

func TestEntryPath(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "roms")
	ok := map[string]string{
		"Game.sfc":      filepath.Join(dest, "Game.sfc"),
		"a/b/Game.sfc":  filepath.Join(dest, "a", "b", "Game.sfc"),
		"a/../Game.sfc": filepath.Join(dest, "Game.sfc"),
		"./Game.sfc":    filepath.Join(dest, "Game.sfc"),
		"..Game.sfc":    filepath.Join(dest, "..Game.sfc"),
		"sub/":          filepath.Join(dest, "sub"),
	}
	for name, want := range ok {
		got, err := entryPath(dest, name)
		if err != nil || got != want {
			t.Errorf("entryPath(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"..", "../Game.sfc", "a/../../Game.sfc"} {
		if _, err := entryPath(dest, name); !errors.Is(err, ErrOutsideDestination) {
			t.Errorf("entryPath(%q) = %v, want ErrOutsideDestination", name, err)
		}
	}
}
