package rnsnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Source says which Hub leg a frame arrived on.
type Source string

const (
	SourceTCP  Source = "tcp"
	SourceMQTT Source = "mqtt"
)

const (
	// IfaceBridgePrefix names the connections on the stunnel listener
	// (bridges with client certificates), IfacePublicPrefix those on the
	// public listener (stock clients), IfaceMQTT the legacy Reticulum topic.
	IfaceBridgePrefix = "tcpb"
	IfacePublicPrefix = "tcpp"
	IfaceMQTT         = "mqtt_rns"

	// echoWindow is how long a frame the node published on the MQTT topic
	// is remembered, so the broker's echo of it to this replica's own
	// subscription is dropped instead of processed as a peer's packet.
	echoWindow = 10 * time.Second
	// persistQueue bounds path writes waiting for the store; an announce
	// storm drops writes (logged) rather than blocking the node.
	persistQueue = 1024
)

// Store is the slice of store.Store the node persists its paths in.
type Store interface {
	UpsertRNSPath(ctx context.Context, p *store.RNSPath) error
	ListRNSPaths(ctx context.Context) ([]store.RNSPath, error)
	DeleteRNSPath(ctx context.Context, destHash string) error
}

// MQTTTransport is the legacy Reticulum MQTT topic as the node sees it.
type MQTTTransport interface {
	Publish(raw []byte) error
	Connected() bool
}

// Config is what New needs.
type Config struct {
	// Identity is the Hub's transport identity (the legacy Hub identity's
	// keys, so the transport id and the meshsat.hub hash do not change).
	Identity *rnswire.Identity
	// Store persists paths; nil keeps them in memory only.
	Store Store
	// PathTTL overrides the upstream default (one week) when > 0.
	PathTTL time.Duration
	// BridgeListenAddr is the stunnel leg (":4242"); PublicListenAddr the
	// public listener, "" for none. PublicIFAC applies to the public one.
	BridgeListenAddr string
	PublicListenAddr string
	PublicIFAC       *rnswire.IFAC
	MaxConns         int
	// MQTT attaches the legacy Reticulum topic as interface IfaceMQTT; nil
	// leaves it out.
	MQTT MQTTTransport
	// OnDeclinedTCP receives every TCP frame the node does not handle (a
	// MeshSat-private packet), for the legacy handler. MQTT frames are
	// handed back by InboundMQTT's return value instead.
	OnDeclinedTCP func(raw []byte)
	// OnAnnounce receives every accepted announce, for the legacy mirror.
	OnAnnounce func(ann *rnswire.Announce, raw []byte, src Source)
	// OnNode is called with the node of each term before its interfaces
	// start, so services register their destinations on it.
	OnNode func(n *rns.Node)
	// OnNodeStopped is called after the term's node is gone, so those
	// services drop what they registered on it.
	OnNodeStopped func()
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Service runs the node for one leadership term at a time: Start on
// acquisition, Stop on loss. Between terms it holds nothing but the config.
type Service struct {
	cfg Config
	now func() time.Time

	mu        sync.RWMutex
	node      *rns.Node
	bridge    *TCPServer
	public    *TCPServer
	cancel    context.CancelFunc
	startedAt time.Time

	active  atomic.Bool
	terms   atomic.Uint64
	persist chan store.RNSPath

	echoMu sync.Mutex
	echo   map[[32]byte]time.Time
}

// New prepares the service; nothing listens until Start.
func New(cfg Config) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, now: now, echo: make(map[[32]byte]time.Time)}
}

// Start builds the node, reloads its paths, binds the listeners and starts
// the node's jobs. It is idempotent while running.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	node, err := rns.New(rns.Config{Identity: s.cfg.Identity, PathTTL: s.cfg.PathTTL, Now: s.cfg.Now}, &transmitter{s: s})
	if err != nil {
		return err
	}
	node.OnAnnounce = func(ann *rnswire.Announce, pkt *rns.Packet) {
		if s.cfg.OnAnnounce != nil {
			src := SourceTCP
			if pkt.Iface == IfaceMQTT {
				src = SourceMQTT
			}
			s.cfg.OnAnnounce(ann, pkt.Raw, src)
		}
	}
	tctx, cancel := context.WithCancel(ctx)
	if s.cfg.Store != nil {
		s.persist = make(chan store.RNSPath, persistQueue)
		s.loadPaths(tctx, node)
		node.OnPathChanged = func(_ [rns.HashLen]byte, e *rns.PathEntry) { s.enqueuePath(e) }
		go s.persister(tctx)
	}
	bridge := NewTCPServer(IfaceBridgePrefix, s.cfg.BridgeListenAddr, s.onTCPFrame)
	bridge.SetMaxConns(s.cfg.MaxConns)
	if err := bridge.Start(); err != nil {
		cancel()
		return err
	}
	var public *TCPServer
	if s.cfg.PublicListenAddr != "" {
		public = NewTCPServer(IfacePublicPrefix, s.cfg.PublicListenAddr, s.onTCPFrame)
		public.SetMaxConns(s.cfg.MaxConns)
		if f := s.cfg.PublicIFAC; f != nil {
			public.OnChange(func(id string, up bool) {
				if up {
					node.SetIFAC(id, f)
				} else {
					node.SetIFAC(id, nil)
				}
			})
		}
		if err := public.Start(); err != nil {
			bridge.Stop()
			cancel()
			return err
		}
	}
	if s.cfg.OnNode != nil {
		s.cfg.OnNode(node)
	}
	node.Start(tctx)
	s.node, s.bridge, s.public, s.cancel, s.startedAt = node, bridge, public, cancel, s.now()
	s.terms.Add(1)
	s.active.Store(true)
	tid := node.IdentityHash()
	slog.Info("rns: upstream-compatible Reticulum node started", "transport_id", hex.EncodeToString(tid[:]),
		"bridge_listener", bridge.Addr(), "public_listener", addrOf(public), "mqtt", s.cfg.MQTT != nil, "paths", node.Paths().Count())
	return nil
}

func addrOf(t *TCPServer) string {
	if t == nil {
		return ""
	}
	return t.Addr()
}

// Stop closes the listeners and the node (every link is torn down). The
// path table is already in the store; the next term reloads it.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel, bridge, public := s.cancel, s.bridge, s.public
	s.node, s.bridge, s.public, s.cancel = nil, nil, nil, nil
	s.active.Store(false)
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	bridge.Stop()
	if public != nil {
		public.Stop()
	}
	if s.cfg.OnNodeStopped != nil {
		s.cfg.OnNodeStopped()
	}
	slog.Info("rns: node stopped")
}

// Active is true while this replica runs the node.
func (s *Service) Active() bool { return s.active.Load() }

// Node returns the running node, or nil between terms.
func (s *Service) Node() *rns.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.node
}

// Listening is true while the stunnel listener is bound.
func (s *Service) Listening() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bridge != nil && s.bridge.Listening()
}

// BridgeCount is the number of connections on the stunnel listener.
func (s *Service) BridgeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.bridge == nil {
		return 0
	}
	return s.bridge.Count()
}

// Broadcast writes a legacy packet to every bridge on the stunnel listener
// (the legacy relay's "tcp" interface); never to the public listener.
func (s *Service) Broadcast(raw []byte) (int, error) {
	s.mu.RLock()
	b := s.bridge
	s.mu.RUnlock()
	if b == nil {
		return 0, errors.New("rnsnode: not running on this replica")
	}
	return b.Broadcast(raw), nil
}

// InboundMQTT hands a frame from the legacy Reticulum topic to the node and
// reports whether it was consumed; false means the legacy handler takes it.
// A frame this node published and the broker echoed back is consumed
// silently. Inactive (not the leader): false, so the legacy layer on every
// replica behaves as it did before the node existed.
func (s *Service) InboundMQTT(raw []byte) bool {
	s.mu.RLock()
	n := s.node
	s.mu.RUnlock()
	if n == nil {
		return false
	}
	if s.isEcho(raw) {
		return true
	}
	return n.Inbound(append([]byte(nil), raw...), IfaceMQTT)
}

func (s *Service) onTCPFrame(ifaceID string, raw []byte) {
	s.mu.RLock()
	n := s.node
	s.mu.RUnlock()
	// The node rewrites the hop byte in place, so it works on a copy and
	// the legacy handler sees the frame as it arrived.
	if n != nil && n.Inbound(append([]byte(nil), raw...), ifaceID) {
		return
	}
	if s.cfg.OnDeclinedTCP != nil {
		s.cfg.OnDeclinedTCP(raw)
	}
}

// --- transmitter ---

type transmitter struct{ s *Service }

func (t *transmitter) Transmit(ifaceID string, raw []byte) error {
	s := t.s
	if ifaceID == IfaceMQTT {
		if s.cfg.MQTT == nil || !s.cfg.MQTT.Connected() {
			return errors.New("rnsnode: mqtt not connected")
		}
		s.noteEcho(raw)
		return s.cfg.MQTT.Publish(raw)
	}
	s.mu.RLock()
	b, p := s.bridge, s.public
	s.mu.RUnlock()
	if b != nil && b.Owns(ifaceID) {
		return b.Send(ifaceID, raw)
	}
	if p != nil && p.Owns(ifaceID) {
		return p.Send(ifaceID, raw)
	}
	return ErrNoSuchInterface
}

func (t *transmitter) Floodable() []string {
	s := t.s
	s.mu.RLock()
	b, p := s.bridge, s.public
	s.mu.RUnlock()
	var out []string
	if b != nil {
		out = append(out, b.Interfaces()...)
	}
	if p != nil {
		out = append(out, p.Interfaces()...)
	}
	if s.cfg.MQTT != nil && s.cfg.MQTT.Connected() {
		out = append(out, IfaceMQTT)
	}
	return out
}

func (t *transmitter) HWMTU(ifaceID string) int {
	if ifaceID == IfaceMQTT {
		return rnswire.MTU
	}
	return TCPHWMTU
}

func (t *transmitter) Bitrate(string) int { return 0 }

// --- MQTT echo suppression ---

func (s *Service) noteEcho(raw []byte) {
	h := sha256.Sum256(raw)
	now := s.now()
	s.echoMu.Lock()
	defer s.echoMu.Unlock()
	for k, at := range s.echo {
		if now.Sub(at) > echoWindow {
			delete(s.echo, k)
		}
	}
	s.echo[h] = now
}

func (s *Service) isEcho(raw []byte) bool {
	h := sha256.Sum256(raw)
	s.echoMu.Lock()
	defer s.echoMu.Unlock()
	at, ok := s.echo[h]
	if !ok {
		return false
	}
	delete(s.echo, h)
	return s.now().Sub(at) <= echoWindow
}

// --- path persistence ---

func (s *Service) enqueuePath(e *rns.PathEntry) {
	if e == nil || e.Announce == nil {
		return
	}
	var blobs []byte
	for _, b := range e.RandomBlobs {
		blobs = append(blobs, b...)
	}
	row := store.RNSPath{DestHash: hex.EncodeToString(e.DestHash[:]), NextHop: hex.EncodeToString(e.NextHop[:]),
		Hops: e.Hops, Iface: e.Iface, ExpiresAt: e.Expires, RandomBlobs: blobs, AnnounceRaw: e.Announce.MarshalPacket(), UpdatedAt: s.now()}
	s.mu.RLock()
	q := s.persist
	s.mu.RUnlock()
	if q == nil {
		return
	}
	select {
	case q <- row:
	default:
		slog.Warn("rns: path persistence queue full, dropping a write", "dest", row.DestHash)
	}
}

func (s *Service) persister(ctx context.Context) {
	s.mu.RLock()
	q := s.persist
	s.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return
		case row := <-q:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.cfg.Store.UpsertRNSPath(wctx, &row); err != nil {
				slog.Warn("rns: persist path failed", "dest", row.DestHash, "error", err)
			}
			cancel()
		}
	}
}

// loadPaths puts every unexpired stored path back into the node and forgets
// the expired ones, so a leader taking over answers path requests at once.
func (s *Service) loadPaths(ctx context.Context, node *rns.Node) {
	rows, err := s.cfg.Store.ListRNSPaths(ctx)
	if err != nil {
		slog.Warn("rns: load paths failed", "error", err)
		return
	}
	now := s.now()
	loaded, expired := 0, 0
	for _, r := range rows {
		if !r.ExpiresAt.After(now) {
			expired++
			_ = s.cfg.Store.DeleteRNSPath(ctx, r.DestHash)
			continue
		}
		ann, err := rnswire.UnmarshalAnnouncePacket(r.AnnounceRaw)
		if err != nil || ann.Verify() != nil {
			continue
		}
		dest, err1 := hex.DecodeString(r.DestHash)
		nh, err2 := hex.DecodeString(r.NextHop)
		if err1 != nil || err2 != nil || len(dest) != rns.HashLen || len(nh) != rns.HashLen {
			continue
		}
		e := &rns.PathEntry{Hops: r.Hops, Expires: r.ExpiresAt, Iface: r.Iface, Announce: ann, Timestamp: now}
		copy(e.DestHash[:], dest)
		copy(e.NextHop[:], nh)
		for i := 0; i+10 <= len(r.RandomBlobs); i += 10 {
			e.RandomBlobs = append(e.RandomBlobs, append([]byte(nil), r.RandomBlobs[i:i+10]...))
		}
		node.Paths().Put(e)
		_ = node.Remember(e.DestHash, ann.PublicKey, ann.AppData)
		loaded++
	}
	if loaded > 0 || expired > 0 {
		slog.Info("rns: paths reloaded from the store", "loaded", loaded, "expired", expired)
	}
}

// --- status ---

// Status describes this replica's node.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{Enabled: true, Active: s.node != nil, Terms: s.terms.Load(), MQTT: s.cfg.MQTT != nil && s.cfg.MQTT.Connected()}
	if s.node == nil {
		return st
	}
	tid := s.node.IdentityHash()
	st.TransportID = hex.EncodeToString(tid[:])
	st.StartedAt = s.startedAt
	st.Bridge = listenerStatus(s.bridge)
	st.Public = listenerStatus(s.public)
	st.Paths = s.node.Paths().Count()
	st.Links = len(s.node.Links().All())
	st.Stats = s.node.Stats()
	return st
}

func listenerStatus(t *TCPServer) *ListenerStatus {
	if t == nil {
		return nil
	}
	return &ListenerStatus{Addr: t.Addr(), Connections: t.Connections(), Stats: t.Stats()}
}

// Paths lists the node's path table (empty when inactive).
func (s *Service) Paths() []PathInfo {
	s.mu.RLock()
	n := s.node
	s.mu.RUnlock()
	out := []PathInfo{}
	if n == nil {
		return out
	}
	for _, e := range n.Paths().All() {
		pi := PathInfo{Dest: hex.EncodeToString(e.DestHash[:]), NextHop: hex.EncodeToString(e.NextHop[:]), Hops: e.Hops,
			Direct: e.Direct(), Iface: e.Iface, Expires: e.Expires}
		if k := n.Recall(e.DestHash); k != nil && len(k.AppData) > 0 {
			pi.AppData = hex.EncodeToString(k.AppData)
		}
		out = append(out, pi)
	}
	return out
}
