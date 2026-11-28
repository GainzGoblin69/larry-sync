package headless

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The device as a Bluetooth gamepad, for playing on a TV with the device in
// hand. BlueZ on the device is started (by the firmware, before this runs)
// without its input plugin, which would otherwise take the HID channels for
// itself, as a host. This then:
//   - names the device, makes it discoverable and pairable, and answers
//     pairing itself (no PIN: "just works");
//   - registers the HID service record, describing a gamepad;
//   - takes the HID control and interrupt channels, as the TV opens them on
//     pairing, or opens them to the TV it last played on;
//   - and sends the buttons the launcher writes to a pipe, as HID reports.

// PadName is what the TV lists the device as.
const PadName = "Larry Boy Advanced"

const (
	hidControlPSM   = 0x11
	hidInterruptPSM = 0x13
	hidUUID         = "00001124-0000-1000-8000-00805f9b34fb"
)

// padReportDescriptor describes the gamepad: report 1, sixteen buttons, a
// hat switch (the D-pad), and four centred axes, which some hosts want
// before they will treat it as a gamepad. Android maps the buttons in order
// to A, B, C, X, Y, Z, L1, R1, L2, R2, Select, Start, Mode, L3, R3.
var padReportDescriptor = []byte{
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x05, // Usage (Game Pad)
	0xA1, 0x01, // Collection (Application)
	0x85, 0x01, //   Report ID (1)
	0x05, 0x09, //   Usage Page (Button)
	0x19, 0x01, //   Usage Minimum (1)
	0x29, 0x10, //   Usage Maximum (16)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x01, //   Logical Maximum (1)
	0x75, 0x01, //   Report Size (1)
	0x95, 0x10, //   Report Count (16)
	0x81, 0x02, //   Input (Data, Variable, Absolute)
	0x05, 0x01, //   Usage Page (Generic Desktop)
	0x09, 0x39, //   Usage (Hat switch)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x07, //   Logical Maximum (7)
	0x35, 0x00, //   Physical Minimum (0)
	0x46, 0x3B, 0x01, // Physical Maximum (315)
	0x65, 0x14, //   Unit (degrees)
	0x75, 0x04, //   Report Size (4)
	0x95, 0x01, //   Report Count (1)
	0x81, 0x42, //   Input (Data, Variable, Absolute, Null state)
	0x65, 0x00, //   Unit (none)
	0x75, 0x04, //   Report Size (4)
	0x95, 0x01, //   Report Count (1)
	0x81, 0x03, //   Input (Constant): padding
	0x09, 0x30, //   Usage (X)
	0x09, 0x31, //   Usage (Y)
	0x09, 0x32, //   Usage (Z)
	0x09, 0x35, //   Usage (Rz)
	0x15, 0x00, //   Logical Minimum (0)
	0x26, 0xFF, 0x00, // Logical Maximum (255)
	0x75, 0x08, //   Report Size (8)
	0x95, 0x04, //   Report Count (4)
	0x81, 0x02, //   Input (Data, Variable, Absolute)
	0xC0, // End Collection
}

// PadState is what the launcher sends for each change: the buttons, as the
// HID report's bits, and the D-pad as a hat (0 up, then clockwise to 7; 8
// for none).
type PadState struct {
	Buttons uint16
	Hat     byte
}

// padNeutral is nothing pressed.
var padNeutral = PadState{Hat: 8}

// report is the HID input report for s, on the interrupt channel.
func (s PadState) report() []byte {
	hat := s.Hat
	if hat > 8 {
		hat = 8
	}
	return []byte{0xA1, 0x01, byte(s.Buttons), byte(s.Buttons >> 8), hat, 0x80, 0x80, 0x80, 0x80}
}

// padRecord is the HID service record, in BlueZ's XML.
func padRecord() string {
	return `<?xml version="1.0" encoding="UTF-8" ?>
<record>
  <attribute id="0x0001"><sequence><uuid value="0x1124" /></sequence></attribute>
  <attribute id="0x0004"><sequence>
    <sequence><uuid value="0x0100" /><uint16 value="0x0011" /></sequence>
    <sequence><uuid value="0x0011" /></sequence>
  </sequence></attribute>
  <attribute id="0x0005"><sequence><uuid value="0x1002" /></sequence></attribute>
  <attribute id="0x0006"><sequence><uint16 value="0x656e" /><uint16 value="0x006a" /><uint16 value="0x0100" /></sequence></attribute>
  <attribute id="0x0009"><sequence><sequence><uuid value="0x1124" /><uint16 value="0x0101" /></sequence></sequence></attribute>
  <attribute id="0x000d"><sequence><sequence>
    <sequence><uuid value="0x0100" /><uint16 value="0x0013" /></sequence>
    <sequence><uuid value="0x0011" /></sequence>
  </sequence></sequence></attribute>
  <attribute id="0x0100"><text value="` + PadName + `" /></attribute>
  <attribute id="0x0101"><text value="Gamepad" /></attribute>
  <attribute id="0x0102"><text value="` + PadName + `" /></attribute>
  <attribute id="0x0200"><uint16 value="0x0100" /></attribute>
  <attribute id="0x0201"><uint16 value="0x0111" /></attribute>
  <attribute id="0x0202"><uint8 value="0x08" /></attribute>
  <attribute id="0x0203"><uint8 value="0x00" /></attribute>
  <attribute id="0x0204"><boolean value="true" /></attribute>
  <attribute id="0x0205"><boolean value="true" /></attribute>
  <attribute id="0x0206"><sequence><sequence>
    <uint8 value="0x22" /><text encoding="hex" value="` + hex.EncodeToString(padReportDescriptor) + `" />
  </sequence></sequence></attribute>
  <attribute id="0x0207"><sequence><sequence><uint16 value="0x0409" /><uint16 value="0x0100" /></sequence></sequence></attribute>
  <attribute id="0x020b"><uint16 value="0x0100" /></attribute>
  <attribute id="0x020c"><uint16 value="0x0c80" /></attribute>
  <attribute id="0x020d"><boolean value="true" /></attribute>
  <attribute id="0x020e"><boolean value="false" /></attribute>
</record>`
}

///////////////////////////////////////
// Bluetooth setup through BlueZ

const (
	padAgentPath   = "/larry/agent"
	padProfilePath = "/larry/hid"
	bluezAdapter   = "/org/bluez/hci0"
)

// setUpBluetooth names the device, makes it findable, answers pairing and
// registers the gamepad. The connection must stay open while it is a pad.
func setUpBluetooth(bus *DBusConn) error {
	// the agent: "just works" pairing, every question answered yes
	bus.Handle(padAgentPath, func(call *dbusMessage) any {
		switch call.member {
		case "RequestPinCode":
			return "0000"
		case "RequestPasskey":
			return uint32(0)
		}
		return nil
	})
	// the profile: the channels are this program's own, so nothing to do
	bus.Handle(padProfilePath, func(*dbusMessage) any { return nil })

	set := func(prop string, value any) error {
		_, err := bus.Call("org.bluez", bluezAdapter, "org.freedesktop.DBus.Properties", "Set",
			"org.bluez.Adapter1", prop, dbusVariant{value})
		if err != nil {
			return fmt.Errorf("setting %s: %w", prop, err)
		}
		return nil
	}
	if err := set("Powered", true); err != nil {
		return err
	}
	_ = set("Alias", PadName)

	if _, err := bus.Call("org.bluez", "/org/bluez", "org.bluez.AgentManager1", "RegisterAgent",
		dbusObjectPath(padAgentPath), "NoInputNoOutput"); err != nil {
		return fmt.Errorf("registering for pairing: %w", err)
	}
	if _, err := bus.Call("org.bluez", "/org/bluez", "org.bluez.AgentManager1", "RequestDefaultAgent",
		dbusObjectPath(padAgentPath)); err != nil {
		return fmt.Errorf("answering pairing: %w", err)
	}
	if _, err := bus.Call("org.bluez", "/org/bluez", "org.bluez.ProfileManager1", "RegisterProfile",
		dbusObjectPath(padProfilePath), hidUUID, dbusDict{
			{"ServiceRecord", padRecord()},
			{"Role", "server"},
			{"RequireAuthentication", false},
			{"RequireAuthorization", false},
		}); err != nil {
		return fmt.Errorf("registering the gamepad: %w", err)
	}

	for _, step := range []struct {
		prop  string
		value any
	}{
		{"DiscoverableTimeout", uint32(0)},
		{"PairableTimeout", uint32(0)},
		{"Pairable", true},
		{"Discoverable", true},
	} {
		if err := set(step.prop, step.value); err != nil {
			return err
		}
	}
	return nil
}

// leadTheLink asks to be the side that leads the Bluetooth link, which can
// send the moment a button is pressed rather than when the TV next asks;
// the Hammer already leads when it was the one that connected. What the
// link ends up as goes to the log, for when latency needs looking into.
func leadTheLink(host string) {
	logger := slog.Default()
	time.Sleep(300 * time.Millisecond) // the connection settles first
	if out, err := exec.Command("hcitool", "sr", host, "master").CombinedOutput(); err != nil {
		logger.Info("Could not lead the Bluetooth link", "host", host, "error", err, "output", strings.TrimSpace(string(out)))
	}
	time.Sleep(500 * time.Millisecond)
	if out, err := exec.Command("hcitool", "con").CombinedOutput(); err == nil {
		logger.Info("Bluetooth link", "connections", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("hcitool", "lp", host).CombinedOutput(); err == nil {
		logger.Info("Bluetooth link policy", "policy", strings.TrimSpace(string(out)))
	}
}

// trust marks the TV as trusted, so it reconnects without asking again.
func trust(bus *DBusConn, addr string) {
	path := bluezAdapter + "/dev_" + strings.ReplaceAll(addr, ":", "_")
	_, _ = bus.Call("org.bluez", path, "org.freedesktop.DBus.Properties", "Set",
		"org.bluez.Device1", "Trusted", dbusVariant{true})
}

///////////////////////////////////////
// L2CAP

const (
	afBluetooth  = 31
	btprotoL2CAP = 0
)

// sockaddrL2 is struct sockaddr_l2.
type sockaddrL2 struct {
	family   uint16
	psm      uint16
	bdaddr   [6]byte // least significant byte first
	cid      uint16
	addrType uint8
	_        uint8
}

func parseBDAddr(addr string) ([6]byte, error) {
	var out [6]byte
	parts := strings.Split(addr, ":")
	if len(parts) != 6 {
		return out, fmt.Errorf("not a Bluetooth address: %q", addr)
	}
	for i, part := range parts {
		b, err := hex.DecodeString(part)
		if err != nil || len(b) != 1 {
			return out, fmt.Errorf("not a Bluetooth address: %q", addr)
		}
		out[5-i] = b[0]
	}
	return out, nil
}

func formatBDAddr(b [6]byte) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[5], b[4], b[3], b[2], b[1], b[0])
}

func l2capSocket() (int, error) {
	return syscall.Socket(afBluetooth, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, btprotoL2CAP)
}

func l2capListen(psm uint16) (int, error) {
	fd, err := l2capSocket()
	if err != nil {
		return -1, fmt.Errorf("no Bluetooth sockets: %w", err)
	}
	sa := sockaddrL2{family: afBluetooth, psm: psm}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); errno != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("taking HID channel %#x: %w (is BlueZ's input plugin still on?)", psm, errno)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func l2capAccept(listener int) (int, string, error) {
	var sa sockaddrL2
	size := uint32(unsafe.Sizeof(sa))
	fd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(listener), uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), syscall.SOCK_CLOEXEC, 0, 0)
	if errno != 0 {
		return -1, "", errno
	}
	return int(fd), formatBDAddr(sa.bdaddr), nil
}

func l2capConnect(addr string, psm uint16) (int, error) {
	bdaddr, err := parseBDAddr(addr)
	if err != nil {
		return -1, err
	}
	fd, err := l2capSocket()
	if err != nil {
		return -1, err
	}
	sa := sockaddrL2{family: afBluetooth, psm: psm, bdaddr: bdaddr}
	if _, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); errno != 0 {
		syscall.Close(fd)
		return -1, errno
	}
	return fd, nil
}

///////////////////////////////////////
// the gamepad

// PadServer is the device as a gamepad.
type PadServer struct {
	Events   *Events
	Pipe     string // where the launcher writes PadStates
	HostFile string // the TV last played on, to reconnect to: "address<TAB>name"
	// New is for pairing another device: the TV last played on is not
	// connected to, even if it is in range.
	New bool

	mu        sync.Mutex
	state     PadState
	control   int
	interrupt int
	host      string
}

// handleControl answers one message from the TV on the control channel.
// Only what a host asks of a gamepad: its report, and setting things it is
// fine to agree to. It returns the reply, and whether the TV unplugged.
func (p *PadServer) handleControl(msg []byte) ([]byte, bool) {
	if len(msg) == 0 {
		return nil, false
	}
	switch msg[0] >> 4 {
	case 0x1: // HID_CONTROL
		if msg[0]&0x0F == 0x5 { // virtual cable unplug
			return nil, true
		}
		return nil, false
	case 0x4: // GET_REPORT
		p.mu.Lock()
		report := p.state.report()
		p.mu.Unlock()
		return report, false
	case 0x5, 0x7, 0x9: // SET_REPORT, SET_PROTOCOL, SET_IDLE
		return []byte{0x00}, false // successful
	case 0x6, 0x8: // GET_PROTOCOL, GET_IDLE
		return []byte{0xA0, 0x01}, false // report protocol / no idle rate
	}
	return []byte{0x03}, false // unsupported request
}

// Serve is a gamepad until ctx ends. It reports RESULT pad waiting, then
// RESULT pad connected and RESULT host <address> while a TV is connected.
func (p *PadServer) Serve(ctx context.Context) error {
	bus, err := DialSystemBus()
	if err != nil {
		return err
	}
	defer bus.Close()
	// BlueZ has just been restarted for this, and takes a moment to appear
	for attempt := 0; ; attempt++ {
		err = setUpBluetooth(bus)
		if err == nil {
			break
		}
		if attempt >= 20 || ctx.Err() != nil {
			return fmt.Errorf("Bluetooth did not start: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	control, err := l2capListen(hidControlPSM)
	if err != nil {
		return err
	}
	defer syscall.Close(control)
	interrupt, err := l2capListen(hidInterruptPSM)
	if err != nil {
		return err
	}
	defer syscall.Close(interrupt)
	go func() {
		<-ctx.Done()
		syscall.Shutdown(control, syscall.SHUT_RDWR)
		syscall.Shutdown(interrupt, syscall.SHUT_RDWR)
		p.disconnect()
	}()

	go p.readPipe(ctx)

	// one loop takes the TV's channels as it opens them, for as long as this
	// runs; connect picks them up
	accepted := make(chan padConn)
	go func() {
		for ctx.Err() == nil {
			c, addr, err := l2capAccept(control)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			i, iaddr, err := l2capAccept(interrupt)
			if err != nil || iaddr != addr {
				syscall.Close(c)
				if err == nil {
					syscall.Close(i)
				}
				continue
			}
			select {
			case accepted <- padConn{c, i, addr}:
			case <-ctx.Done():
				syscall.Close(c)
				syscall.Close(i)
				return
			}
		}
	}()

	p.Events.Result("pad", "waiting")
	for ctx.Err() == nil {
		host, ok := p.connect(ctx, accepted)
		if !ok {
			continue
		}
		trust(bus, host)
		name := deviceName(bus, host)
		_ = os.WriteFile(p.HostFile, []byte(host+"\t"+name+"\n"), 0o644)
		p.New = false // this one is the TV now
		p.Events.Result("host", host)
		if name != "" {
			p.Events.Result("host_name", name)
		}
		p.Events.Result("pad", "connected")
		go leadTheLink(host)
		p.send()
		p.serveControl()
		if ctx.Err() == nil {
			p.Events.Result("pad", "waiting")
		}
	}
	return nil
}

// readHost is the TV last played on: its address and name.
func readHost(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	addr, name, _ := strings.Cut(strings.TrimSpace(string(data)), "\t")
	return strings.TrimSpace(addr), strings.TrimSpace(name)
}

// deviceName is what the TV calls itself, from BlueZ, or "" if unknown.
func deviceName(bus *DBusConn, addr string) string {
	path := bluezAdapter + "/dev_" + strings.ReplaceAll(addr, ":", "_")
	for _, prop := range []string{"Alias", "Name"} {
		reply, err := bus.Call("org.bluez", path, "org.freedesktop.DBus.Properties", "Get", "org.bluez.Device1", prop)
		if err != nil || reply.signature != "v" || len(reply.body) < 3 {
			continue
		}
		sigLen := int(reply.body[0])
		if sigLen != 1 || reply.body[1] != 's' {
			continue
		}
		if name, _, err := decodeDBusString(reply.body, 3); err == nil && name != "" {
			return name
		}
	}
	return ""
}

// padConn is a TV's two channels.
type padConn struct {
	control, interrupt int
	addr               string
}

// connect waits for the TV to open both channels, or opens them to the TV
// last played on, every few seconds. Returns the TV's address.
func (p *PadServer) connect(ctx context.Context, accepted <-chan padConn) (string, bool) {
	last, _ := readHost(p.HostFile)
	tryLast := func() (string, bool) {
		if last == "" || p.New {
			return "", false
		}
		c, err := l2capConnect(last, hidControlPSM)
		if err != nil {
			return "", false
		}
		i, err := l2capConnect(last, hidInterruptPSM)
		if err != nil {
			syscall.Close(c)
			return "", false
		}
		p.setChannels(c, i, last)
		return last, true
	}
	if host, ok := tryLast(); ok {
		return host, true
	}
	retry := time.NewTicker(4 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case c := <-accepted:
			p.setChannels(c.control, c.interrupt, c.addr)
			return c.addr, true
		case <-retry.C:
			if host, ok := tryLast(); ok {
				return host, true
			}
		}
	}
}

func (p *PadServer) setChannels(control, interrupt int, host string) {
	// button reports go ahead of anything else waiting for the radio
	if interrupt > 0 {
		_ = syscall.SetsockoptInt(interrupt, syscall.SOL_SOCKET, syscall.SO_PRIORITY, 6)
	}
	p.mu.Lock()
	p.control, p.interrupt, p.host = control, interrupt, host
	p.mu.Unlock()
}

func (p *PadServer) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.control > 0 {
		syscall.Shutdown(p.control, syscall.SHUT_RDWR)
		syscall.Close(p.control)
	}
	if p.interrupt > 0 {
		syscall.Shutdown(p.interrupt, syscall.SHUT_RDWR)
		syscall.Close(p.interrupt)
	}
	p.control, p.interrupt, p.host = 0, 0, ""
}

// serveControl answers the TV on the control channel until it goes.
func (p *PadServer) serveControl() {
	p.mu.Lock()
	control := p.control
	p.mu.Unlock()
	buf := make([]byte, 64)
	for {
		n, err := syscall.Read(control, buf)
		if err != nil || n <= 0 {
			break
		}
		reply, unplug := p.handleControl(buf[:n])
		if unplug {
			break
		}
		if reply != nil {
			if _, err := syscall.Write(control, reply); err != nil {
				break
			}
		}
	}
	p.disconnect()
}

// send sends the buttons now, if a TV is there.
func (p *PadServer) send() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.interrupt <= 0 {
		return
	}
	_, _ = syscall.Write(p.interrupt, p.state.report())
}

// readPipe takes the launcher's button changes: two bytes of buttons (low
// first) and one of hat each.
func (p *PadServer) readPipe(ctx context.Context) {
	_ = syscall.Mkfifo(p.Pipe, 0o600)
	// read and write, so it never sees the launcher's end close
	file, err := os.OpenFile(p.Pipe, os.O_RDWR, 0)
	if err != nil {
		p.Events.Error(fmt.Errorf("cannot read the buttons: %w", err))
		return
	}
	go func() {
		<-ctx.Done()
		file.Close()
	}()
	buf := make([]byte, 3)
	for {
		if _, err := io.ReadFull(file, buf); err != nil {
			return
		}
		p.mu.Lock()
		p.state = PadState{Buttons: uint16(buf[0]) | uint16(buf[1])<<8, Hat: buf[2]}
		p.mu.Unlock()
		p.send()
	}
}

// NewPadServer returns a gamepad keeping its files beside the catalog.
func NewPadServer(home string, events *Events) *PadServer {
	return &PadServer{
		Events:   events,
		Pipe:     "/tmp/larry-pad.fifo",
		HostFile: filepath.Join(home, "pad-host"),
		state:    padNeutral,
	}
}
