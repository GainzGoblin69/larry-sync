package headless

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Updates to the firmware come from the GitHub releases of the repository it
// was built from. A release carries the firmware as MinUI.zip; put at the top
// of the card, the boot script installs it on the next start, keeping games,
// saves and settings.

// UpdateFile is the release asset that is the firmware, and its name on the
// card for the boot script.
const UpdateFile = "MinUI.zip"

// ErrNoUpdateSource means the firmware does not say where its releases are.
var ErrNoUpdateSource = errors.New("this build does not say where its updates come from")

// ErrNoUpdateAsset means the newest release carries no MinUI.zip.
var ErrNoUpdateAsset = errors.New("the newest release has no " + UpdateFile)

// Updater checks for and fetches firmware updates.
type Updater struct {
	Card   string // the top of the SD card
	Events *Events
	// API is GitHub's API address; tests point it elsewhere.
	API    string
	Client *http.Client
}

// NewUpdater returns an Updater for the card at card.
func NewUpdater(card string, events *Events) *Updater {
	return &Updater{
		Card:   card,
		Events: events,
		API:    "https://api.github.com",
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Installed is what the firmware on the card says of itself, from
// .system/version.txt (build name, then commit) and .system/larry-repo.txt.
type Installed struct {
	Name   string
	Commit string
	Repo   string
}

// Release is the newest published firmware.
type Release struct {
	Tag       string
	Name      string
	Published time.Time
	Asset     releaseAsset
}

type releaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"` // "sha256:<hex>", when GitHub has worked it out
}

// ReadInstalled reads what the firmware on the card says of itself.
func (u *Updater) ReadInstalled() Installed {
	var in Installed
	if data, err := os.ReadFile(filepath.Join(u.Card, ".system", "version.txt")); err == nil {
		lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
		if len(lines) > 0 {
			in.Name = strings.TrimSpace(lines[0])
		}
		if len(lines) > 1 {
			in.Commit = strings.TrimSpace(lines[1])
		}
	}
	if data, err := os.ReadFile(filepath.Join(u.Card, ".system", "larry-repo.txt")); err == nil {
		in.Repo = strings.TrimSpace(string(data))
	}
	return in
}

// Latest asks GitHub for the newest release of repo.
func (u *Updater) Latest(ctx context.Context, repo string) (Release, error) {
	if repo == "" || strings.Count(repo, "/") != 1 {
		return Release{}, ErrNoUpdateSource
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.API+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "larry-sync")
	resp, err := u.Client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}

	var body struct {
		TagName     string         `json:"tag_name"`
		Name        string         `json:"name"`
		PublishedAt time.Time      `json:"published_at"`
		Assets      []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("reading the release: %w", err)
	}
	release := Release{Tag: body.TagName, Name: body.Name, Published: body.PublishedAt}
	for _, asset := range body.Assets {
		if asset.Name == UpdateFile {
			release.Asset = asset
		}
	}
	return release, nil
}

// IsNewer reports whether release is a different build from the one
// installed. Release tags end in the commit they were built from, as
// "build-20261126-main-0-027f4c0", and the installed build names its commit.
// An installed build that names none is always offered the release.
func IsNewer(installed Installed, release Release) bool {
	if release.Tag == "" {
		return false
	}
	commit := strings.ToLower(installed.Commit)
	if commit == "" {
		return true
	}
	tagCommit := strings.ToLower(release.Tag[strings.LastIndex(release.Tag, "-")+1:])
	if tagCommit == "" {
		return true
	}
	return !strings.HasPrefix(commit, tagCommit) && !strings.HasPrefix(tagCommit, commit)
}

// Check reports the installed build and the newest release:
// RESULT current, latest, latest_date, size, available (1 or 0).
func (u *Updater) Check(ctx context.Context) (Installed, Release, error) {
	installed := u.ReadInstalled()
	u.Events.Stage("check", "")
	release, err := u.Latest(ctx, installed.Repo)
	if err != nil {
		return installed, release, err
	}
	current := installed.Name
	if current == "" {
		current = "unknown"
	}
	u.Events.Result("current", current)
	u.Events.Result("latest", release.Tag)
	if !release.Published.IsZero() {
		u.Events.Result("latest_date", release.Published.Local().Format("Jan 2, 2006"))
	}
	u.Events.Result("size", fmt.Sprint(release.Asset.Size))
	available := IsNewer(installed, release) && release.Asset.URL != ""
	u.Events.Result("available", boolField(available))
	return installed, release, nil
}

// Download fetches the newest release's firmware to the top of the card,
// where the boot script installs it. It is written beside it first and only
// takes its name once whole and checked, so a cut-off download is never
// installed. RESULT ready <path>.
func (u *Updater) Download(ctx context.Context) error {
	installed := u.ReadInstalled()
	release, err := u.Latest(ctx, installed.Repo)
	if err != nil {
		return err
	}
	if release.Asset.URL == "" {
		return ErrNoUpdateAsset
	}

	dest := filepath.Join(u.Card, UpdateFile)
	partial := dest + ".part"
	defer os.Remove(partial)

	u.Events.Stage("update", release.Tag)
	if err := u.fetch(ctx, release.Asset, partial); err != nil {
		return err
	}
	if err := checkFirmware(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, dest); err != nil {
		return fmt.Errorf("putting the update in place: %w", err)
	}
	u.Events.Result("ready", dest)
	return nil
}

func (u *Updater) fetch(ctx context.Context, asset releaseAsset, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "larry-sync")
	// A large file on a slow connection outlasts the API timeout; ctx still
	// stops it when cancelled.
	client := *u.Client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the update: GitHub answered %s", resp.Status)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("writing the update: %w", err)
	}
	total := asset.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	hash := sha256.New()
	progress := &updateProgress{events: u.Events, total: total}
	_, err = io.Copy(io.MultiWriter(file, hash, progress), resp.Body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("downloading the update: %w", err)
	}
	if asset.Size > 0 && progress.done != asset.Size {
		return fmt.Errorf("the update was cut short (%d of %d bytes)", progress.done, asset.Size)
	}
	if want, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok && want != "" {
		if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, want) {
			return errors.New("the update did not arrive intact; try again")
		}
	}
	return nil
}

// checkFirmware makes sure the file is a whole firmware zip: one that opens
// and holds the system folder the boot script installs.
func checkFirmware(path string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("the update is not a whole zip: %w", err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if strings.HasPrefix(file.Name, ".system/") {
			return nil
		}
	}
	return errors.New("the update does not hold the firmware")
}

type updateProgress struct {
	events *Events
	done   int64
	total  int64
}

func (p *updateProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	p.events.Progress(p.done, p.total)
	return len(b), nil
}
