package headless

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"grout/cache"
	"grout/cfw"
	"grout/files"
	"grout/romm"
	"grout/settings"
)

// ExportDirEnv overrides where the catalog files and state are written.
const ExportDirEnv = "LARRY_DIR"

var (
	// ErrNotPaired means there is no server token yet. The caller should run
	// the pairing flow before anything else.
	ErrNotPaired = errors.New("not paired with a RomM server")
	// ErrUnreachable means the server did not answer. Everything that works
	// from the local catalog still can.
	ErrUnreachable = errors.New("RomM server is unreachable")
	// ErrNotInCatalog means the game is not in the local catalog, which a
	// catalog sync fixes.
	ErrNotInCatalog = errors.New("game is not in the local catalog")
)

// Session is one command's view of the device: the stored settings, the open
// catalog and where to report.
type Session struct {
	Config *settings.Config
	Host   settings.Host
	CFW    cfw.CFW
	Events *Events
	// ExportDir holds the catalog files the launcher reads, and the state.
	ExportDir string
	State     State
}

// ExportDir is where the catalog files go: LARRY_DIR, or a larry folder beside
// the settings.
func ExportDir() string {
	if dir := os.Getenv(ExportDirEnv); dir != "" {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join(os.TempDir(), "larry")
	}
	return filepath.Join(wd, "larry")
}

// Open loads the settings and the catalog. It needs no network.
func Open(events *Events) (*Session, error) {
	active, err := cfw.Active()
	if err != nil {
		return nil, err
	}

	config, err := settings.LoadConfig()
	if err != nil || len(config.Hosts) == 0 || !config.Hosts[0].HasTokenAuth() {
		return nil, ErrNotPaired
	}
	host := config.Hosts[0]

	dir := ExportDir()
	state, err := LoadState(dir)
	if err != nil {
		// A damaged state file costs the offline folder mapping and the record
		// of unpacked games, both of which the next sync and fetch rebuild.
		slog.Default().Warn("Ignoring unreadable state file", "dir", dir, "error", err)
		state = State{}
	}
	if len(config.PlatformsBinding) == 0 {
		config.PlatformsBinding = state.PlatformsBinding
	}

	if err := cache.InitCacheManager(host, *config); err != nil {
		return nil, fmt.Errorf("opening the catalog: %w", err)
	}

	return &Session{
		Config:    config,
		Host:      host,
		CFW:       active,
		Events:    events,
		ExportDir: dir,
		State:     state,
	}, nil
}

// Close releases the catalog and clears temporary files.
func (s *Session) Close() {
	if cm := cache.GetCacheManager(); cm != nil {
		if err := cm.Close(); err != nil {
			slog.Default().Warn("Failed to close the catalog", "error", err)
		}
	}
	if err := os.RemoveAll(files.TempDir()); err != nil {
		slog.Default().Warn("Failed to clear temporary files", "error", err)
	}
}

func (s *Session) client() *romm.Client {
	return romm.NewClientFromHost(s.Host, s.Config.ApiTimeout.Duration())
}

// Online checks that the server answers and still accepts the stored token.
func (s *Session) Online() error {
	probe := romm.NewClient(s.Host.URL(),
		romm.WithInsecureSkipVerify(s.Host.InsecureSkipVerify),
		romm.WithTimeout(settings.ValidationTimeout))
	if err := probe.ValidateConnection(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	err := romm.NewClientFromHost(s.Host, settings.LoginTimeout).ValidateToken()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, romm.ErrUnauthorized), errors.Is(err, romm.ErrForbidden):
		// Only a refusal means pairing again will help. A timeout here is the
		// network, and re-pairing over it would throw away a good token.
		return fmt.Errorf("%w: the server rejected the stored token: %v", ErrNotPaired, err)
	default:
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
}

func (s *Session) saveState() {
	if err := s.State.Save(s.ExportDir); err != nil {
		slog.Default().Warn("Failed to save state", "dir", s.ExportDir, "error", err)
	}
}

// catalogManager returns the open catalog or an error a caller can report.
func catalogManager() (*cache.Manager, error) {
	cm := cache.GetCacheManager()
	if cm == nil {
		return nil, errors.New("the catalog is not open")
	}
	return cm, nil
}
