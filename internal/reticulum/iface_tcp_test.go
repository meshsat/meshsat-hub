package reticulum

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTCPInterface_SendReceive(t *testing.T) {
	iface := NewTCPInterface("127.0.0.1:0") // OS picks free port

	var received []byte
	var mu sync.Mutex
	done := make(chan struct{})

	iface.SetHandler(func(_ InterfaceType, raw []byte) {
		mu.Lock()
		received = raw
		mu.Unlock()
		close(done)
	})

	if err := iface.Start(); err != nil {
		t.Fatal(err)
	}
	defer iface.Stop()

	addr := iface.listener.Addr().String()

	// Connect a client
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Wait for client to be registered
	time.Sleep(50 * time.Millisecond)
	if iface.ClientCount() != 1 {
		t.Errorf("client count: got %d, want 1", iface.ClientCount())
	}

	// Send an HDLC frame from client
	packet := make([]byte, 25)
	for i := range packet {
		packet[i] = byte(i + 1)
	}
	frame := HDLCFrame(packet)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}

	// Wait for handler
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for packet")
	}

	mu.Lock()
	if !bytes.Equal(received, packet) {
		t.Errorf("received: got %x, want %x", received, packet)
	}
	mu.Unlock()
}

func TestTCPInterface_Broadcast(t *testing.T) {
	iface := NewTCPInterface("127.0.0.1:0")
	iface.SetHandler(func(_ InterfaceType, _ []byte) {})

	if err := iface.Start(); err != nil {
		t.Fatal(err)
	}
	defer iface.Stop()

	addr := iface.listener.Addr().String()

	// Connect two clients
	conn1, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn1.Close() }()

	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close() }()

	time.Sleep(50 * time.Millisecond)
	if iface.ClientCount() != 2 {
		t.Errorf("client count: got %d, want 2", iface.ClientCount())
	}

	// Broadcast a packet
	packet := make([]byte, 20)
	for i := range packet {
		packet[i] = byte(i + 100)
	}
	if err := iface.Send(context.TODO(), "", packet); err != nil {
		t.Fatal(err)
	}

	// Both clients should receive the HDLC frame
	expected := HDLCFrame(packet)
	for i, conn := range []net.Conn{conn1, conn2} {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, len(expected)+10)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("client %d: read error: %v", i, err)
		}
		if !bytes.Equal(buf[:n], expected) {
			t.Errorf("client %d: got %x, want %x", i, buf[:n], expected)
		}
	}
}

func TestTCPInterface_ClientDisconnect(t *testing.T) {
	iface := NewTCPInterface("127.0.0.1:0")
	iface.SetHandler(func(_ InterfaceType, _ []byte) {})

	if err := iface.Start(); err != nil {
		t.Fatal(err)
	}
	defer iface.Stop()

	addr := iface.listener.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	if iface.ClientCount() != 1 {
		t.Errorf("before disconnect: got %d, want 1", iface.ClientCount())
	}

	_ = conn.Close()
	time.Sleep(200 * time.Millisecond)

	// Send should not panic with no clients
	if err := iface.Send(context.TODO(), "", make([]byte, 20)); err != nil {
		t.Errorf("send with no clients should not error: %v", err)
	}
}

func TestTCPInterface_Name(t *testing.T) {
	iface := NewTCPInterface("127.0.0.1:4242")
	if iface.Name() != IfaceTCP {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceTCP)
	}
	if iface.Cost() != 0 {
		t.Errorf("cost: got %f, want 0", iface.Cost())
	}
	if iface.MTU() != MTU {
		t.Errorf("mtu: got %d, want %d", iface.MTU(), MTU)
	}
}
