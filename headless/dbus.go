package headless

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A small D-Bus client, enough to talk to BlueZ: method calls with strings,
// object paths, booleans, numbers, variants and string-to-variant dicts, and
// answering the calls BlueZ makes back (a pairing agent, a profile) with an
// empty reply or a fixed answer. Messages are little-endian, as BlueZ's are.

const (
	dbusMethodCall   = 1
	dbusMethodReturn = 2
	dbusError        = 3
	dbusSignal       = 4

	dbusNoReplyExpected = 0x1
)

// dbusObjectPath is a value sent as an object path ("o").
type dbusObjectPath string

// dbusVariant is a value sent as a variant ("v").
type dbusVariant struct{ value any }

// dbusDict is a string-to-variant dict ("a{sv}"), sent in key order given.
type dbusDict []dbusEntry

type dbusEntry struct {
	key   string
	value any
}

// dbusMessage is one message, decoded as far as this client needs.
type dbusMessage struct {
	kind        byte
	flags       byte
	serial      uint32
	replySerial uint32
	path        string
	iface       string
	member      string
	errorName   string
	sender      string
	signature   string
	body        []byte
}

// dbusCall is a method call BlueZ made to one of this client's objects.
// The handler's return is the reply: nil for an empty one, or one value.
type dbusHandler func(call *dbusMessage) any

// DBusConn is a connection to the system bus.
type DBusConn struct {
	conn     net.Conn
	reader   *bufio.Reader
	writeMu  sync.Mutex
	serial   uint32
	mu       sync.Mutex
	waiting  map[uint32]chan *dbusMessage
	handlers map[string]dbusHandler // by object path
	closed   chan struct{}
}

// DialSystemBus connects to the system bus and says hello.
func DialSystemBus() (*DBusConn, error) {
	var conn net.Conn
	var err error
	for _, path := range []string{"/run/dbus/system_bus_socket", "/var/run/dbus/system_bus_socket"} {
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cannot reach the system bus: %w", err)
	}
	return newDBusConn(conn, strconv.Itoa(os.Getuid()))
}

func newDBusConn(conn net.Conn, uid string) (*DBusConn, error) {
	c := &DBusConn{
		conn:     conn,
		reader:   bufio.NewReader(conn),
		waiting:  map[uint32]chan *dbusMessage{},
		handlers: map[string]dbusHandler{},
		closed:   make(chan struct{}),
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("\x00AUTH EXTERNAL " + hex.EncodeToString([]byte(uid)) + "\r\n")); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("the system bus refused this program: %s", strings.TrimSpace(line))
	}
	if _, err := conn.Write([]byte("BEGIN\r\n")); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	go c.readLoop()
	if _, err := c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close ends the connection; BlueZ forgets the agent and profile with it.
func (c *DBusConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return c.conn.Close()
}

// Handle answers calls BlueZ makes to path.
func (c *DBusConn) Handle(path string, handler dbusHandler) {
	c.mu.Lock()
	c.handlers[path] = handler
	c.mu.Unlock()
}

// Call makes a method call and waits for its reply.
func (c *DBusConn) Call(dest, path, iface, member string, args ...any) (*dbusMessage, error) {
	c.mu.Lock()
	c.serial++
	serial := c.serial
	reply := make(chan *dbusMessage, 1)
	c.waiting[serial] = reply
	c.mu.Unlock()

	msg, err := encodeDBusMessage(dbusMethodCall, 0, serial, 0, path, iface, member, dest, "", args)
	if err != nil {
		return nil, err
	}
	if err := c.write(msg); err != nil {
		return nil, err
	}
	select {
	case m := <-reply:
		if m.kind == dbusError {
			text := ""
			if m.signature != "" && m.signature[0] == 's' {
				if s, _, err := decodeDBusString(m.body, 0); err == nil {
					text = s
				}
			}
			return m, &DBusError{Name: m.errorName, Message: text}
		}
		return m, nil
	case <-time.After(25 * time.Second):
		c.mu.Lock()
		delete(c.waiting, serial)
		c.mu.Unlock()
		return nil, fmt.Errorf("no answer to %s.%s", iface, member)
	case <-c.closed:
		return nil, errors.New("the system bus connection closed")
	}
}

// DBusError is an error reply.
type DBusError struct{ Name, Message string }

func (e *DBusError) Error() string {
	if e.Message == "" {
		return e.Name
	}
	return e.Name + ": " + e.Message
}

func (c *DBusConn) write(msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write(msg)
	return err
}

func (c *DBusConn) readLoop() {
	defer c.Close()
	for {
		m, err := readDBusMessage(c.reader)
		if err != nil {
			return
		}
		switch m.kind {
		case dbusMethodReturn, dbusError:
			c.mu.Lock()
			reply := c.waiting[m.replySerial]
			delete(c.waiting, m.replySerial)
			c.mu.Unlock()
			if reply != nil {
				reply <- m
			}
		case dbusMethodCall:
			c.answer(m)
		}
	}
}

// answer replies to a call BlueZ made to one of this client's objects.
func (c *DBusConn) answer(call *dbusMessage) {
	c.mu.Lock()
	handler := c.handlers[call.path]
	c.serial++
	serial := c.serial
	c.mu.Unlock()
	if call.flags&dbusNoReplyExpected != 0 && handler == nil {
		return
	}

	var msg []byte
	var err error
	if handler == nil {
		msg, err = encodeDBusMessage(dbusError, 0, serial, call.serial, "", "", "", call.sender,
			"org.freedesktop.DBus.Error.UnknownObject", []any{"no such object"})
	} else {
		var args []any
		if value := handler(call); value != nil {
			args = []any{value}
		}
		msg, err = encodeDBusMessage(dbusMethodReturn, 0, serial, call.serial, "", "", "", call.sender, "", args)
	}
	if err == nil && call.flags&dbusNoReplyExpected == 0 {
		_ = c.write(msg)
	}
}

///////////////////////////////////////
// encoding

type dbusEncoder struct{ buf bytes.Buffer }

func (e *dbusEncoder) align(n int) {
	for e.buf.Len()%n != 0 {
		e.buf.WriteByte(0)
	}
}

func (e *dbusEncoder) uint32(v uint32) {
	e.align(4)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf.Write(b[:])
}

func (e *dbusEncoder) str(s string) {
	e.uint32(uint32(len(s)))
	e.buf.WriteString(s)
	e.buf.WriteByte(0)
}

func (e *dbusEncoder) sig(s string) {
	e.buf.WriteByte(byte(len(s)))
	e.buf.WriteString(s)
	e.buf.WriteByte(0)
}

// signature of one value
func dbusSignatureOf(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return "s", nil
	case dbusObjectPath:
		return "o", nil
	case bool:
		return "b", nil
	case uint32:
		return "u", nil
	case int32:
		return "i", nil
	case uint16:
		return "q", nil
	case byte:
		return "y", nil
	case dbusVariant:
		return "v", nil
	case dbusDict:
		return "a{sv}", nil
	case []byte:
		return "ay", nil
	default:
		return "", fmt.Errorf("cannot send a %T over D-Bus", v)
	}
}

func (e *dbusEncoder) value(v any) error {
	switch v := v.(type) {
	case string:
		e.str(v)
	case dbusObjectPath:
		e.str(string(v))
	case bool:
		if v {
			e.uint32(1)
		} else {
			e.uint32(0)
		}
	case uint32:
		e.uint32(v)
	case int32:
		e.uint32(uint32(v))
	case uint16:
		e.align(2)
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		e.buf.Write(b[:])
	case byte:
		e.buf.WriteByte(v)
	case dbusVariant:
		sig, err := dbusSignatureOf(v.value)
		if err != nil {
			return err
		}
		e.sig(sig)
		return e.value(v.value)
	case []byte:
		e.uint32(uint32(len(v)))
		e.buf.Write(v)
	case dbusDict:
		e.align(4)
		lengthAt := e.buf.Len()
		e.buf.Write([]byte{0, 0, 0, 0})
		e.align(8) // dict entries are structs
		start := e.buf.Len()
		for _, entry := range v {
			e.align(8)
			e.str(entry.key)
			if err := e.value(dbusVariant{entry.value}); err != nil {
				return err
			}
		}
		binary.LittleEndian.PutUint32(e.buf.Bytes()[lengthAt:], uint32(e.buf.Len()-start))
	default:
		return fmt.Errorf("cannot send a %T over D-Bus", v)
	}
	return nil
}

func encodeDBusMessage(kind byte, flags byte, serial, replySerial uint32, path, iface, member, dest, errorName string, args []any) ([]byte, error) {
	var body dbusEncoder
	signature := ""
	for _, arg := range args {
		sig, err := dbusSignatureOf(arg)
		if err != nil {
			return nil, err
		}
		signature += sig
		if err := body.value(arg); err != nil {
			return nil, err
		}
	}

	var h dbusEncoder
	h.buf.Write([]byte{'l', kind, flags, 1})
	h.uint32(uint32(body.buf.Len()))
	h.uint32(serial)
	fieldsAt := h.buf.Len()
	h.buf.Write([]byte{0, 0, 0, 0})
	fieldsStart := h.buf.Len() // already 8-aligned (16)
	field := func(code byte, sig string, write func()) {
		h.align(8)
		h.buf.WriteByte(code)
		h.sig(sig)
		write()
	}
	if path != "" {
		field(1, "o", func() { h.str(path) })
	}
	if iface != "" {
		field(2, "s", func() { h.str(iface) })
	}
	if member != "" {
		field(3, "s", func() { h.str(member) })
	}
	if errorName != "" {
		field(4, "s", func() { h.str(errorName) })
	}
	if replySerial != 0 {
		field(5, "u", func() { h.uint32(replySerial) })
	}
	if dest != "" {
		field(6, "s", func() { h.str(dest) })
	}
	if signature != "" {
		field(8, "g", func() { h.sig(signature) })
	}
	binary.LittleEndian.PutUint32(h.buf.Bytes()[fieldsAt:], uint32(h.buf.Len()-fieldsStart))
	h.align(8)
	h.buf.Write(body.buf.Bytes())
	return h.buf.Bytes(), nil
}

///////////////////////////////////////
// decoding

func readDBusMessage(r io.Reader) (*dbusMessage, error) {
	head := make([]byte, 16)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	if head[0] != 'l' {
		return nil, errors.New("only little-endian D-Bus messages are understood")
	}
	bodyLen := binary.LittleEndian.Uint32(head[4:8])
	fieldsLen := binary.LittleEndian.Uint32(head[12:16])
	if bodyLen > 1<<24 || fieldsLen > 1<<20 {
		return nil, errors.New("a D-Bus message too large")
	}
	padded := (16 + int(fieldsLen) + 7) &^ 7
	rest := make([]byte, padded-16+int(bodyLen))
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	full := append(head, rest...)
	m := &dbusMessage{
		kind:   head[1],
		flags:  head[2],
		serial: binary.LittleEndian.Uint32(head[8:12]),
		body:   full[padded:],
	}
	pos := 16
	end := 16 + int(fieldsLen)
	for pos < end {
		pos = (pos + 7) &^ 7
		if pos >= end {
			break
		}
		code := full[pos]
		pos++
		sigLen := int(full[pos])
		sig := string(full[pos+1 : pos+1+sigLen])
		pos += 1 + sigLen + 1
		switch sig {
		case "s", "o":
			s, next, err := decodeDBusString(full, pos)
			if err != nil {
				return nil, err
			}
			pos = next
			switch code {
			case 1:
				m.path = s
			case 2:
				m.iface = s
			case 3:
				m.member = s
			case 4:
				m.errorName = s
			case 7:
				m.sender = s
			}
		case "u":
			pos = (pos + 3) &^ 3
			if pos+4 > len(full) {
				return nil, errors.New("a short D-Bus header")
			}
			v := binary.LittleEndian.Uint32(full[pos:])
			pos += 4
			if code == 5 {
				m.replySerial = v
			}
		case "g":
			n := int(full[pos])
			m.signature = string(full[pos+1 : pos+1+n])
			pos += 1 + n + 1
		default:
			return nil, fmt.Errorf("a D-Bus header field of type %q", sig)
		}
	}
	return m, nil
}

// decodeDBusString reads a string at pos in buf, which is aligned as a
// message is (the body starts 8-aligned, so either works).
func decodeDBusString(buf []byte, pos int) (string, int, error) {
	pos = (pos + 3) &^ 3
	if pos+4 > len(buf) {
		return "", 0, errors.New("a short D-Bus string")
	}
	n := int(binary.LittleEndian.Uint32(buf[pos:]))
	pos += 4
	if pos+n+1 > len(buf) {
		return "", 0, errors.New("a short D-Bus string")
	}
	return string(buf[pos : pos+n]), pos + n + 1, nil
}
