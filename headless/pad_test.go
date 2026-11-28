package headless

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPadReport(t *testing.T) {
	if got := padNeutral.report(); !bytes.Equal(got, []byte{0xA1, 0x01, 0, 0, 8, 0x80, 0x80, 0x80, 0x80}) {
		t.Errorf("nothing pressed: % x", got)
	}
	// B (button 1) and Start (button 12), D-pad up-right
	s := PadState{Buttons: 1<<0 | 1<<11, Hat: 1}
	if got := s.report(); !bytes.Equal(got[:5], []byte{0xA1, 0x01, 0x01, 0x08, 0x01}) {
		t.Errorf("B + Start, up-right: % x", got)
	}
	if got := (PadState{Hat: 200}).report(); got[4] != 8 {
		t.Errorf("an impossible hat is sent as none, got %d", got[4])
	}
}

func TestPadReportDescriptorIsWhole(t *testing.T) {
	// every collection closed, and the input reports add up to the 8 bytes sent
	// after the report ID: 16 buttons, 4-bit hat, 4 bits padding, 4 axes of 8
	opens, closes, bits := 0, 0, 0
	size, count := 0, 0
	d := padReportDescriptor
	for i := 0; i < len(d); {
		prefix := d[i]
		n := int(prefix & 0x3)
		if n == 3 {
			n = 4
		}
		var value int
		for k := 0; k < n; k++ {
			value |= int(d[i+1+k]) << (8 * k)
		}
		switch prefix & 0xFC {
		case 0xA0:
			opens++
		case 0xC0:
			closes++
		case 0x74:
			size = value
		case 0x94:
			count = value
		case 0x80:
			bits += size * count
		}
		i += 1 + n
	}
	if opens != closes || opens == 0 {
		t.Errorf("collections: %d opened, %d closed", opens, closes)
	}
	if bits != 8*(len(padNeutral.report())-2) {
		t.Errorf("the descriptor describes %d bits, the report sends %d", bits, 8*(len(padNeutral.report())-2))
	}
}

func TestPadRecordCarriesTheDescriptor(t *testing.T) {
	record := padRecord()
	if !strings.Contains(record, hex.EncodeToString(padReportDescriptor)) {
		t.Error("the service record does not carry the report descriptor")
	}
	if !strings.Contains(record, `<uint8 value="0x08" />`) {
		t.Error("the service record does not say gamepad")
	}
}

func TestPadControl(t *testing.T) {
	p := &PadServer{state: PadState{Buttons: 2, Hat: 4}}
	cases := []struct {
		msg    []byte
		reply  []byte
		unplug bool
	}{
		{[]byte{0x41}, PadState{Buttons: 2, Hat: 4}.report(), false}, // GET_REPORT
		{[]byte{0x70}, []byte{0x00}, false},                          // SET_PROTOCOL boot? still fine
		{[]byte{0x71}, []byte{0x00}, false},                          // SET_PROTOCOL report
		{[]byte{0x52, 0x01, 0x00}, []byte{0x00}, false},              // SET_REPORT
		{[]byte{0x60}, []byte{0xA0, 0x01}, false},                    // GET_PROTOCOL
		{[]byte{0x15}, nil, true},                                    // virtual cable unplug
		{[]byte{0xF0}, []byte{0x03}, false},                          // unknown
		{nil, nil, false},
	}
	for _, c := range cases {
		reply, unplug := p.handleControl(c.msg)
		if !bytes.Equal(reply, c.reply) || unplug != c.unplug {
			t.Errorf("% x: got % x %v, want % x %v", c.msg, reply, unplug, c.reply, c.unplug)
		}
	}
}

func TestBDAddr(t *testing.T) {
	b, err := parseBDAddr("A4:C1:38:0B:5E:F2")
	if err != nil {
		t.Fatal(err)
	}
	if b != [6]byte{0xF2, 0x5E, 0x0B, 0x38, 0xC1, 0xA4} {
		t.Errorf("% x", b)
	}
	if got := formatBDAddr(b); got != "A4:C1:38:0B:5E:F2" {
		t.Errorf("round trip: %s", got)
	}
	if _, err := parseBDAddr("not:an:addr"); err == nil {
		t.Error("parsed a bad address")
	}
}

// The launcher's button changes, through the pipe, come out on the
// interrupt channel as reports, in order, as they happen.
func TestPadSendsTheLaunchersButtons(t *testing.T) {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(pair[1])
	p := NewPadServer(t.TempDir(), NewEvents(&bytes.Buffer{}))
	p.Pipe = filepath.Join(t.TempDir(), "pad.fifo")
	p.setChannels(-1, pair[0], "00:11:22:33:44:55")
	defer p.disconnect()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.readPipe(ctx)
	var launcher *os.File
	for i := 0; i < 100; i++ {
		if launcher, err = os.OpenFile(p.Pipe, os.O_WRONLY, 0); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if launcher == nil {
		t.Fatalf("no pipe: %v", err)
	}
	defer launcher.Close()

	presses := []PadState{
		{Buttons: 1 << 0, Hat: 8},       // A
		{Buttons: 1<<0 | 1<<11, Hat: 0}, // A + Start, up
		{Buttons: 0, Hat: 6},            // left
		padNeutral,
	}
	for _, s := range presses {
		launcher.Write([]byte{byte(s.Buttons), byte(s.Buttons >> 8), s.Hat})
	}
	buf := make([]byte, 32)
	for i, s := range presses {
		syscall.SetsockoptTimeval(pair[1], syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Sec: 2})
		n, err := syscall.Read(pair[1], buf)
		if err != nil {
			t.Fatalf("report %d did not arrive: %v", i, err)
		}
		if !bytes.Equal(buf[:n], s.report()) {
			t.Errorf("report %d: % x, want % x", i, buf[:n], s.report())
		}
	}
}
