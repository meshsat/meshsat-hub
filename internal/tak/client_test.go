package tak

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// startSilentServer returns a listener and a channel that yields each
// accepted connection. The server never writes on its own — it mimics an
// OTS plain-TCP endpoint, which relays nothing back (IFRNLLEI01PRD-905).
func startSilentServer(t *testing.T) (net.Listener, chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conns := make(chan net.Conn, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- conn
		}
	}()
	return ln, conns
}

func clientFor(t *testing.T, ln net.Listener) *Client {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return NewClient("127.0.0.1", port, false)
}

// An event arriving after an idle period must still be delivered. With the
// old per-read deadline, any 60s-idle window made bufio.Scanner's error
// sticky: inbound data was never read again and the loop span at 100% CPU.
func TestClientDeliversAfterIdle(t *testing.T) {
	ln, conns := startSilentServer(t)
	defer func() { _ = ln.Close() }()

	c := clientFor(t, ln)
	events := make(chan CotEvent, 1)
	c.SetEventHandler(func(ev CotEvent) { events <- ev })

	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Disconnect()

	serverConn := <-conns
	defer func() { _ = serverConn.Close() }()

	time.Sleep(300 * time.Millisecond) // idle window before any traffic

	data, err := MarshalCotEvent(BuildSOSEvent("idle-test-001", "IDLE-1", 51.5, -0.12, 600))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := serverConn.Write(append(data, '\n')); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case ev := <-events:
		if ev.UID != "idle-test-001" {
			t.Errorf("uid: got %q, want %q", ev.UID, "idle-test-001")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered after idle period")
	}
}

// Disconnect must unblock a readLoop that is waiting on a silent
// connection and return promptly.
func TestClientDisconnectUnblocksIdleRead(t *testing.T) {
	ln, conns := startSilentServer(t)
	defer func() { _ = ln.Close() }()

	c := clientFor(t, ln)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	serverConn := <-conns
	defer func() { _ = serverConn.Close() }()

	time.Sleep(100 * time.Millisecond) // let readLoop block on the idle conn

	done := make(chan struct{})
	go func() {
		c.Disconnect()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect did not unblock the idle readLoop")
	}
}
