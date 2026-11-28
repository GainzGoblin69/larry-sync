package headless

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"grout/romm"
	"grout/settings"
)

// partSuffix marks a file that is still arriving. The launcher lists what is
// in a rom folder, so a half-written game must not carry a name it would try
// to launch.
const partSuffix = ".part"

// transfer downloads one file from the RomM server.
type transfer struct {
	client *http.Client
	auth   string
}

// newTransfer returns a transfer for host. timeout bounds the whole download,
// so it has to allow for the largest game on a slow link.
func newTransfer(host settings.Host, timeout time.Duration) *transfer {
	return &transfer{
		client: romm.NewHTTPClient(host, timeout),
		auth:   host.AuthHeader(),
	}
}

// fetch writes the file at source to dest, reporting bytes as they arrive.
//
// The file is written beside dest under a temporary name and renamed once it
// is complete. An interrupted download leaves nothing at dest, so a game is
// either wholly present or not there at all. expected is the size to report
// against when the server does not send one, and may be 0.
func (t *transfer) fetch(ctx context.Context, source, dest string, expected int64, onProgress func(done, total int64)) error {
	// Paths carry the file's name, which routinely has spaces.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.ReplaceAll(source, " ", "%20"), nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if t.auth != "" {
		request.Header.Set("Authorization", t.auth)
	}

	response, err := t.client.Do(request)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", filepath.Base(dest), err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("server answered %s for %s", response.Status, filepath.Base(dest))
	}

	total := response.ContentLength
	if total <= 0 {
		total = expected
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
	}

	partial := dest + partSuffix
	out, err := os.Create(partial)
	if err != nil {
		return fmt.Errorf("creating %s: %w", partial, err)
	}

	counter := &progressWriter{total: total, onProgress: onProgress}
	_, copyErr := io.Copy(out, io.TeeReader(response.Body, counter))
	closeErr := out.Close()

	if copyErr != nil {
		os.Remove(partial)
		return fmt.Errorf("downloading %s: %w", filepath.Base(dest), copyErr)
	}
	if closeErr != nil {
		os.Remove(partial)
		return fmt.Errorf("writing %s: %w", filepath.Base(dest), closeErr)
	}

	// A connection that drops cleanly mid-body can look like the end of the
	// file. The size is the only check available before a game is launched.
	if response.ContentLength > 0 && counter.done != response.ContentLength {
		os.Remove(partial)
		return fmt.Errorf("downloading %s: got %d of %d bytes", filepath.Base(dest), counter.done, response.ContentLength)
	}

	if err := os.Rename(partial, dest); err != nil {
		os.Remove(partial)
		return fmt.Errorf("moving %s into place: %w", filepath.Base(dest), err)
	}
	return nil
}

// progressWriter counts bytes on their way to the file.
type progressWriter struct {
	done       int64
	total      int64
	onProgress func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if w.onProgress != nil {
		w.onProgress(w.done, w.total)
	}
	return len(p), nil
}
