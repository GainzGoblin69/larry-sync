package headless

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"grout/bios"
	"grout/files"
	"grout/romm"
)

// BIOSReport says what a BIOS check found and did.
type BIOSReport struct {
	// Present counts files that were already on the device.
	Present int
	// Installed counts files downloaded by this check.
	Installed int
	// Failed counts files that could not be downloaded or saved.
	Failed int
}

// EnsureBIOS downloads every BIOS file the server holds for a platform that is
// not on the device yet.
//
// Grout asks which files to fetch. Here there is nobody to ask, and the files
// are small, so everything missing is fetched: a file uploaded to the server
// for this platform was put there to be used.
func (s *Session) EnsureBIOS(ctx context.Context, platform romm.Platform) (BIOSReport, error) {
	var report BIOSReport

	if platform.ID == 0 {
		return report, fmt.Errorf("platform %q has no id in the catalog", platform.FSSlug)
	}

	firmware, err := s.client().GetFirmware(platform.ID)
	if err != nil {
		return report, fmt.Errorf("listing BIOS files: %w", err)
	}

	var missing []bios.Requirement
	for _, requirement := range bios.Match(platform.FSSlug, firmware) {
		if requirement.Installed(platform.FSSlug) {
			report.Present++
			continue
		}
		missing = append(missing, requirement)
	}
	if len(missing) == 0 {
		s.Events.Result("bios_installed", "0")
		return report, nil
	}

	s.Events.Stage("bios", strconv.Itoa(len(missing)))
	fetcher := newTransfer(s.Host, s.Config.DownloadTimeout.Duration())

	for i, requirement := range missing {
		if err := s.installBIOS(ctx, fetcher, platform, requirement); err != nil {
			report.Failed++
			slog.Default().Warn("Failed to install BIOS file",
				"file", requirement.Firmware.FileName, "platform", platform.FSSlug, "error", err)
			s.Events.Info("BIOS file %s could not be installed: %v", requirement.Firmware.FileName, err)
		} else {
			report.Installed++
		}
		s.Events.Progress(int64(i+1), int64(len(missing)))
	}

	s.Events.Result("bios_installed", strconv.Itoa(report.Installed))
	if report.Failed > 0 {
		s.Events.Result("bios_failed", strconv.Itoa(report.Failed))
	}
	return report, nil
}

// installBIOS downloads one file to a temporary name and then writes it
// everywhere the firmware looks for it.
func (s *Session) installBIOS(ctx context.Context, fetcher *transfer, platform romm.Platform, requirement bios.Requirement) error {
	staged := filepath.Join(files.TempDir(),
		fmt.Sprintf("bios_%d_%s", requirement.Firmware.ID, bios.SafeFileName(requirement.Firmware.FileName)))
	defer os.Remove(staged)

	source := s.Host.URL() + requirement.Firmware.DownloadURL
	if err := fetcher.fetch(ctx, source, staged, requirement.Firmware.FileSizeBytes, nil); err != nil {
		return err
	}

	data, err := os.ReadFile(staged)
	if err != nil {
		return fmt.Errorf("reading the downloaded file: %w", err)
	}
	return requirement.Save(platform.FSSlug, data)
}

// PlatformBySlug finds a platform in the local catalog.
func (s *Session) PlatformBySlug(fsSlug string) (romm.Platform, error) {
	cm, err := catalogManager()
	if err != nil {
		return romm.Platform{}, err
	}
	platforms, err := cm.GetPlatforms()
	if err != nil {
		return romm.Platform{}, fmt.Errorf("reading platforms: %w", err)
	}
	for _, platform := range platforms {
		if platform.FSSlug == fsSlug {
			return platform, nil
		}
	}
	return romm.Platform{}, fmt.Errorf("%w: platform %q", ErrNotInCatalog, fsSlug)
}
