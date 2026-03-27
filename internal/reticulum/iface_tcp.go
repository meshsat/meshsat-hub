package reticulum

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// TCPInterface implements Interface for raw Reticulum TCP transport.
// It accepts inbound connections from RNS nodes and bridges using HDLC framing
// (wire-compatible with Python RNS TCPClientInterface and MeshSat Bridge).
//
// Packets received from any TCP client are forwarded to the relay.
// Packets sent via Send() are broadcast to all connected clients.
type TCPInterface struct {
	mu       sync.RWMutex
	addr     string
	listener net.Listener
	clients  map[uint64]*tcpClient
	handler  PacketHandler
	nextID   uint64
	done     chan struct{}
}

type tcpClient struct {
	id     uint64
	conn   net.Conn
	writer sync.Mutex
}

// NewTCPInterface creates a TCP Reticulum interface that listens on the given address.
func NewTCPInterface(listenAddr string) *TCPInterface {
	return &TCPInterface{
		addr:    listenAddr,
		clients: make(map[uint64]*tcpClient),
		done:    make(chan struct{}),
	}
}

// Name returns the interface type.
func (t *TCPInterface) Name() InterfaceType { return IfaceTCP }

// Cost returns zero (TCP is free).
func (t *TCPInterface) Cost() float64 { return 0 }

// MTU returns the Reticulum MTU.
func (t *TCPInterface) MTU() int { return MTU }

// Send broadcasts an HDLC-framed packet to all connected TCP clients.
func (t *TCPInterface) Send(_ context.Context, _ string, packet []byte) error {
	frame := HDLCFrame(packet)

	t.mu.RLock()
	clients := make([]*tcpClient, 0, len(t.clients))
	for _, c := range t.clients {
		clients = append(clients, c)
	}
	t.mu.RUnlock()

	if len(clients) > 0 {
		slog.Debug("reticulum: tcp sending packet",
			"packet_size", len(packet), "frame_size", len(frame),
			"clients", len(clients))
	}

	for _, c := range clients {
		c.writer.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		n, err := c.conn.Write(frame)
		c.writer.Unlock()
		if err != nil {
			slog.Debug("reticulum: tcp write failed, dropping client",
				"client", c.id, "error", err)
			t.removeClient(c.id)
		} else {
			slog.Debug("reticulum: tcp frame sent",
				"client", c.id, "bytes_written", n)
		}
	}
	return nil
}

// IsAvailable returns true if the listener is active.
func (t *TCPInterface) IsAvailable() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.listener != nil
}

// SetHandler registers a callback for inbound Reticulum packets.
func (t *TCPInterface) SetHandler(h PacketHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

// ClientCount returns the number of connected TCP clients.
func (t *TCPInterface) ClientCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.clients)
}

// Start begins listening for TCP connections.
func (t *TCPInterface) Start() error {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return fmt.Errorf("reticulum tcp: listen %s: %w", t.addr, err)
	}

	t.mu.Lock()
	t.listener = ln
	t.mu.Unlock()

	slog.Info("reticulum: tcp interface listening", "addr", t.addr)
	go t.acceptLoop(ln)
	return nil
}

// Stop closes the listener and all client connections.
func (t *TCPInterface) Stop() {
	close(t.done)

	t.mu.Lock()
	if t.listener != nil {
		_ = t.listener.Close()
		t.listener = nil
	}
	for id, c := range t.clients {
		_ = c.conn.Close()
		delete(t.clients, id)
	}
	t.mu.Unlock()

	slog.Info("reticulum: tcp interface stopped")
}

func (t *TCPInterface) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-t.done:
				return
			default:
				slog.Error("reticulum: tcp accept error", "error", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		t.mu.Lock()
		t.nextID++
		id := t.nextID
		client := &tcpClient{id: id, conn: conn}
		t.clients[id] = client
		t.mu.Unlock()

		slog.Info("reticulum: tcp client connected",
			"client", id, "remote", conn.RemoteAddr())
		go t.readLoop(client)
	}
}

func (t *TCPInterface) readLoop(client *tcpClient) {
	defer func() {
		t.removeClient(client.id)
		_ = client.conn.Close()
		slog.Info("reticulum: tcp client disconnected",
			"client", client.id, "remote", client.conn.RemoteAddr())
	}()

	reader := NewHDLCFrameReader()
	buf := make([]byte, 4096)

	for {
		select {
		case <-t.done:
			return
		default:
		}

		_ = client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := client.conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				select {
				case <-t.done:
				default:
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						// Read timeout is normal (keepalive), continue
						continue
					}
					slog.Debug("reticulum: tcp read error",
						"client", client.id, "error", err)
				}
			}
			return
		}

		frames := reader.Feed(buf[:n])
		for _, frame := range frames {
			t.mu.RLock()
			h := t.handler
			t.mu.RUnlock()

			if h != nil {
				slog.Debug("reticulum: tcp packet received",
					"client", client.id, "size", len(frame))
				h(IfaceTCP, frame)
			}
		}
	}
}

func (t *TCPInterface) removeClient(id uint64) {
	t.mu.Lock()
	if c, ok := t.clients[id]; ok {
		_ = c.conn.Close()
		delete(t.clients, id)
	}
	t.mu.Unlock()
}
