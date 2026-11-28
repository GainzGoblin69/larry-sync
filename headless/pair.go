package headless

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"grout/auth"
	"grout/cfw"
	"grout/romm"
	"grout/saves"
	"grout/settings"
)

// PairOptions says which server to pair with.
type PairOptions struct {
	// URL is the server's address as typed, scheme and port included.
	URL string
	// DeviceName is how the device shows up in RomM.
	DeviceName string
	// InsecureSkipVerify accepts a self-signed certificate.
	InsecureSkipVerify bool
}

// Pair links this device to a RomM server.
//
// It reports the address to open as RESULT pair_url, then waits for the user
// to approve the device in a browser. The launcher shows that address as a QR
// code. On approval the token is stored where grout keeps it, so grout itself
// is paired by the same act.
func Pair(ctx context.Context, events *Events, options PairOptions) error {
	// Pairing tells the server which firmware this is, so a misconfigured
	// launch script should fail here rather than register a nameless device.
	if _, err := cfw.Active(); err != nil {
		return err
	}

	root := strings.TrimRight(strings.TrimSpace(options.URL), "/")
	if root == "" {
		return errors.New("no server address given")
	}
	if !strings.HasPrefix(root, "http://") && !strings.HasPrefix(root, "https://") {
		root = "http://" + root
	}

	name := strings.TrimSpace(options.DeviceName)
	if name == "" {
		name = "Larry Boy Advanced"
	}

	config, err := settings.LoadConfig()
	if err != nil {
		config = &settings.Config{ShowRegularCollections: true}
	}

	// Pairing the same server again keeps the identifier it knows this device
	// by, so the server updates its record instead of adding a second device.
	host := settings.Host{RootURI: root, InsecureSkipVerify: options.InsecureSkipVerify}
	if len(config.Hosts) > 0 && config.Hosts[0].URL() == root {
		host = config.Hosts[0]
		host.InsecureSkipVerify = options.InsecureSkipVerify
	}

	events.Stage("connect", root)
	capabilities, err := auth.Connect(host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if !capabilities.SupportsDevicePairing {
		return errors.New("this server does not offer device pairing; RomM 5.0 or later is needed")
	}

	pairing, err := auth.StartPairing(host, name)
	if err != nil {
		return fmt.Errorf("starting pairing: %w", err)
	}

	events.Stage("approve", name)
	events.Result("pair_url", pairing.VerificationURL)

	result := pairing.Wait(ctx)
	switch result.Outcome {
	case auth.PairingApproved:
	case auth.PairingDenied:
		return errors.New("pairing was denied on the server")
	case auth.PairingExpired:
		return errors.New("pairing expired before it was approved")
	case auth.PairingCancelled:
		return errors.New("pairing was cancelled")
	default:
		if result.Err != nil {
			return fmt.Errorf("pairing failed: %w", result.Err)
		}
		return errors.New("pairing failed")
	}

	host = result.Host

	// Save sync addresses the device by the id the server issued. A token
	// that came back without one still works for browsing and downloading, so
	// registering is attempted but does not fail the pairing.
	if host.DeviceID == "" {
		device, err := saves.RegisterDevice(romm.NewClientFromHost(host, settings.LoginTimeout), name)
		if err != nil {
			events.Info("Paired, but the device could not be registered for save sync: %v", err)
		} else {
			host.DeviceID = device.ID
			host.DeviceName = name
		}
	}

	config.Hosts = []settings.Host{host}
	if err := settings.SaveConfig(config); err != nil {
		return fmt.Errorf("saving the pairing: %w", err)
	}

	events.Result("paired", host.URL())
	events.Result("username", host.Username)
	return nil
}
