package headless

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// testBus starts a private dbus-daemon and returns its socket. Skipped where
// there is none.
func testBus(t *testing.T) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon here")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "bus")
	config := filepath.Join(dir, "bus.conf")
	os.WriteFile(config, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=`+socket+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>`), 0o644)
	cmd := exec.Command(daemon, "--config-file="+config, "--nofork")
	if err := cmd.Start(); err != nil {
		t.Skipf("dbus-daemon would not start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the bus did not start")
	return ""
}

func dialTestBus(t *testing.T, socket string) *DBusConn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := newDBusConn(conn, strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

// A fake BlueZ on a real bus takes the gamepad's setup, and the gamepad
// answers the pairing questions BlueZ asks back.
func TestPadSetsUpThroughBlueZ(t *testing.T) {
	socket := testBus(t)
	bluez := dialTestBus(t, socket)
	if _, err := bluez.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "RequestName", "org.bluez", uint32(0)); err != nil {
		t.Fatalf("taking org.bluez: %v", err)
	}
	var mu sync.Mutex
	var calls []string
	var sender string
	record := func(call *dbusMessage) any {
		mu.Lock()
		calls = append(calls, call.member+"("+call.signature+")")
		sender = call.sender
		mu.Unlock()
		return nil
	}
	bluez.Handle("/org/bluez", record)
	bluez.Handle("/org/bluez/hci0", record)

	pad := dialTestBus(t, socket)
	if err := setUpBluetooth(pad); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	from := sender
	mu.Unlock()
	want := []string{
		"Set(ssv)", "Set(ssv)", // Powered, Alias
		"RegisterAgent(os)", "RequestDefaultAgent(o)",
		"RegisterProfile(osa{sv})",
		"Set(ssv)", "Set(ssv)", "Set(ssv)", "Set(ssv)",
	}
	if len(got) != len(want) {
		t.Fatalf("BlueZ was asked %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %s, want %s", i, got[i], want[i])
		}
	}

	// BlueZ asks the agent for a PIN (an old TV): it answers 0000
	reply, err := bluez.Call(from, padAgentPath, "org.bluez.Agent1", "RequestPinCode", dbusObjectPath("/org/bluez/hci0/dev_00_11_22_33_44_55"))
	if err != nil {
		t.Fatalf("asking the agent: %v", err)
	}
	pin, _, err := decodeDBusString(reply.body, 0)
	if err != nil || pin != "0000" || reply.signature != "s" {
		t.Errorf("PIN %q (%q, %v)", pin, reply.signature, err)
	}
	// and agrees to the rest
	if _, err := bluez.Call(from, padAgentPath, "org.bluez.Agent1", "RequestConfirmation", dbusObjectPath("/org/bluez/hci0/dev_00_11_22_33_44_55"), uint32(123456)); err != nil {
		t.Errorf("confirming: %v", err)
	}
	if _, err := bluez.Call(from, "/nowhere", "org.bluez.Agent1", "Release"); err == nil {
		t.Error("an object it does not have answered")
	}
}
