// Package rnsnode assembles the Hub's upstream-compatible Reticulum node
// (internal/rns) on the Hub's own interfaces: the TCP listeners and the
// legacy Reticulum MQTT topic, with the path table persisted in the store.
// One node runs, on the leader (MESHSAT-1555); cmd/meshsat-hub/main.go
// starts it on leadership acquired and stops it on loss.
package rnsnode

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

const (
	// TCPHWMTU is what upstream's TCP interfaces report as HW_MTU.
	TCPHWMTU = 262144
	// DefaultMaxConns bounds one listener; a refused connection is closed at
	// once and counted.
	DefaultMaxConns = 512

	tcpWriteTimeout = 10 * time.Second
	tcpReadIdle     = 60 * time.Second
	tcpKeepAlive    = 10 * time.Second
)

// ErrNoSuchInterface is returned for an interface id that names no live
// connection: the peer went away, or the id was persisted before a restart.
var ErrNoSuchInterface = errors.New("rnsnode: no such interface")

// TCPServer is one listener. Every accepted connection is its own node
// interface, named "<name>_<n>", as upstream's TCPServerInterface spawns one
// interface per client: a path learned from a client is reachable through
// that client's connection and no other.
type TCPServer struct {
	name     string
	addr     string
	maxConns int
	onFrame  func(ifaceID string, raw []byte)
	onChange func(ifaceID string, connected bool)

	mu    sync.RWMutex
	ln    net.Listener
	conns map[string]*tcpConn
	next  uint64
	done  chan struct{}

	accepted, refused, rxFrames, txFrames, dropped atomic.Uint64
}

type tcpConn struct {
	id     string
	remote string
	c      net.Conn
	since  time.Time
	wmu    sync.Mutex
	rx, tx atomic.Uint64
}

// ConnInfo describes one live connection for the status API.
type ConnInfo struct {
	ID       string    `json:"id"`
	Remote   string    `json:"remote"`
	Since    time.Time `json:"since"`
	RxFrames uint64    `json:"rx_frames"`
	TxFrames uint64    `json:"tx_frames"`
}

// TCPStats are the listener's counters.
type TCPStats struct {
	Accepted, Refused, RxFrames, TxFrames, Dropped uint64
}

// NewTCPServer prepares a listener on addr whose connections are named
// "<name>_<n>". onFrame receives every HDLC frame with the connection's
// interface id.
func NewTCPServer(name, addr string, onFrame func(ifaceID string, raw []byte)) *TCPServer {
	return &TCPServer{name: name, addr: addr, maxConns: DefaultMaxConns, onFrame: onFrame,
		conns: make(map[string]*tcpConn), done: make(chan struct{})}
}

// SetMaxConns bounds the number of simultaneous connections (0 keeps the default).
func (s *TCPServer) SetMaxConns(n int) {
	if n > 0 {
		s.maxConns = n
	}
}

// OnChange is called after a connection is added (connected true) and after
// it is removed; the node uses it to apply a per-interface access code.
func (s *TCPServer) OnChange(fn func(ifaceID string, connected bool)) { s.onChange = fn }

// Owns reports whether ifaceID names a connection of this listener (live or not).
func (s *TCPServer) Owns(ifaceID string) bool { return strings.HasPrefix(ifaceID, s.name+"_") }

// Start binds the listener and accepts in the background.
func (s *TCPServer) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("rnsnode: listen %s: %w", s.addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	slog.Info("rns: tcp listener up", "name", s.name, "addr", ln.Addr().String())
	go s.acceptLoop(ln)
	return nil
}

// Addr is the bound address ("" before Start).
func (s *TCPServer) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Listening reports whether the listener is bound.
func (s *TCPServer) Listening() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ln != nil
}

// Stop closes the listener and every connection.
func (s *TCPServer) Stop() {
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
	conns := make([]*tcpConn, 0, len(s.conns))
	for id, c := range s.conns {
		conns = append(conns, c)
		delete(s.conns, id)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.c.Close()
	}
	slog.Info("rns: tcp listener stopped", "name", s.name)
}

// Send writes one packet, HDLC framed, to the named connection.
func (s *TCPServer) Send(ifaceID string, raw []byte) error {
	s.mu.RLock()
	c := s.conns[ifaceID]
	s.mu.RUnlock()
	if c == nil {
		return ErrNoSuchInterface
	}
	return s.write(c, rnswire.HDLCFrame(raw))
}

// Broadcast writes one packet to every connection and returns how many took
// it. The legacy relay's view of "the TCP interface" (every bridge on
// stunnel at once); the node itself never broadcasts, it names an interface.
func (s *TCPServer) Broadcast(raw []byte) int {
	frame := rnswire.HDLCFrame(raw)
	s.mu.RLock()
	conns := make([]*tcpConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.RUnlock()
	n := 0
	for _, c := range conns {
		if s.write(c, frame) == nil {
			n++
		}
	}
	return n
}

func (s *TCPServer) write(c *tcpConn, frame []byte) error {
	c.wmu.Lock()
	_ = c.c.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
	_, err := c.c.Write(frame)
	c.wmu.Unlock()
	if err != nil {
		s.dropped.Add(1)
		slog.Debug("rns: tcp write failed, dropping connection", "iface", c.id, "error", err)
		s.remove(c.id)
		_ = c.c.Close()
		return err
	}
	c.tx.Add(1)
	s.txFrames.Add(1)
	slog.Debug("rns: tcp frame written", "iface", c.id, "bytes", len(frame))
	return nil
}

// Interfaces lists the live connection ids, sorted.
func (s *TCPServer) Interfaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.conns))
	for id := range s.conns {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Connections describes the live connections, sorted by id.
func (s *TCPServer) Connections() []ConnInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ConnInfo, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, ConnInfo{ID: c.id, Remote: c.remote, Since: c.since, RxFrames: c.rx.Load(), TxFrames: c.tx.Load()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Count is the number of live connections.
func (s *TCPServer) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.conns)
}

// Stats returns the listener's counters.
func (s *TCPServer) Stats() TCPStats {
	return TCPStats{Accepted: s.accepted.Load(), Refused: s.refused.Load(), RxFrames: s.rxFrames.Load(),
		TxFrames: s.txFrames.Load(), Dropped: s.dropped.Load()}
}

func (s *TCPServer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			slog.Warn("rns: tcp accept error", "name", s.name, "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(tcpKeepAlive)
			_ = tc.SetNoDelay(true)
		}
		s.mu.Lock()
		if len(s.conns) >= s.maxConns {
			s.mu.Unlock()
			s.refused.Add(1)
			_ = conn.Close()
			slog.Warn("rns: tcp connection refused, listener full", "name", s.name, "max", s.maxConns)
			continue
		}
		s.next++
		c := &tcpConn{id: fmt.Sprintf("%s_%d", s.name, s.next), remote: conn.RemoteAddr().String(), c: conn, since: time.Now()}
		s.conns[c.id] = c
		s.mu.Unlock()
		s.accepted.Add(1)
		slog.Info("rns: peer connected", "iface", c.id, "remote", c.remote)
		if s.onChange != nil {
			s.onChange(c.id, true)
		}
		go s.readLoop(c)
	}
}

func (s *TCPServer) readLoop(c *tcpConn) {
	defer func() {
		if s.remove(c.id) && s.onChange != nil {
			s.onChange(c.id, false)
		}
		_ = c.c.Close()
		slog.Info("rns: peer disconnected", "iface", c.id, "remote", c.remote, "rx", c.rx.Load(), "tx", c.tx.Load())
	}()
	reader := rnswire.NewHDLCFrameReader()
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		_ = c.c.SetReadDeadline(time.Now().Add(tcpReadIdle))
		n, err := c.c.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // idle is normal; TCP keepalive finds a dead peer
			}
			if !errors.Is(err, io.EOF) {
				slog.Debug("rns: tcp read ended", "iface", c.id, "error", err)
			}
			return
		}
		for _, frame := range reader.Feed(buf[:n]) {
			c.rx.Add(1)
			s.rxFrames.Add(1)
			s.onFrame(c.id, frame)
		}
	}
}

// remove forgets a connection; true when it was still registered.
func (s *TCPServer) remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.conns[id]; !ok {
		return false
	}
	delete(s.conns, id)
	return true
}
