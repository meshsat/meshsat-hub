package oob

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Bearer names.
const (
	BearerMQTT = "mqtt"
	BearerSMS  = "sms"
	BearerIMT  = "imt"
	BearerSBD  = "sbd"
)

// Transport delivers one frame text to a bridge over one bearer.
type Transport interface {
	// Send delivers text (the "MS:..." form) to the peer's address on this
	// bearer. It returns an error when the peer has no address for it.
	Send(ctx context.Context, tenantID string, peer *store.OOBPeer, text string) error
	// Timeout is how long a reply may take on this bearer.
	Timeout() time.Duration
}

// Reply is a decoded reply to a command the Hub sent.
type Reply struct {
	Bearer   string     `json:"bearer"`
	RC       ResultCode `json:"rc"`
	Result   string     `json:"result"`
	Body     string     `json:"body"`
	Counter  uint32     `json:"counter"`
	Seq      byte       `json:"seq"`
	Total    byte       `json:"total"`
	Received time.Time  `json:"received"`
}

// Store is the slice of store.Store the service uses.
type Store interface {
	UpsertOOBPeer(ctx context.Context, p *store.OOBPeer) error
	GetOOBPeer(ctx context.Context, tenantID string, bridgeID string) (*store.OOBPeer, error)
	ListOOBPeersByPeerID(ctx context.Context, peerID int) ([]store.OOBPeer, error)
	DeleteOOBPeer(ctx context.Context, tenantID string, bridgeID string) error
	NextOOBCounter(ctx context.Context, tenantID string, bridgeID string) (int64, error)
	SetOOBReplayWindow(ctx context.Context, tenantID string, bridgeID string, high int64, window int64) error
}

// Service pairs the Hub with bridges as an OOB peer, seals and sends command
// frames over a bearer, classifies inbound frames and correlates replies.
type Service struct {
	store      Store
	masterKey  []byte
	audit      *audit.Service
	encrypt    bool
	maxPerHour int

	mu         sync.Mutex
	transports map[string]Transport
	pending    map[string]chan Reply // bridge|counterLo16 -> waiter
	sent       map[string][]time.Time
	now        func() time.Time
}

// Options tunes the service.
type Options struct {
	Encrypt    bool // seal args encrypted (default true)
	MaxPerHour int  // outbound frames per bridge per bearer per hour (default 20)
}

// New creates the service. masterKey encrypts peer keys at rest.
func New(s Store, masterKey []byte, auditSvc *audit.Service, opts Options) *Service {
	if opts.MaxPerHour <= 0 {
		opts.MaxPerHour = 20
	}
	return &Service{store: s, masterKey: masterKey, audit: auditSvc, encrypt: opts.Encrypt, maxPerHour: opts.MaxPerHour,
		transports: map[string]Transport{}, pending: map[string]chan Reply{}, sent: map[string][]time.Time{}, now: time.Now}
}

// RegisterTransport attaches a bearer.
func (s *Service) RegisterTransport(bearer string, t Transport) {
	s.mu.Lock()
	s.transports[bearer] = t
	s.mu.Unlock()
}

// Bearers lists the registered bearers.
func (s *Service) Bearers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.transports))
	for b := range s.transports {
		out = append(out, b)
	}
	return out
}

// Errors.
var (
	ErrNotPaired  = errors.New("oob: bridge is not paired (no management key)")
	ErrNoBearer   = errors.New("oob: no usable bearer for this bridge")
	ErrRateLimit  = errors.New("oob: outbound rate limit reached for this bridge and bearer")
	ErrTimeout    = errors.New("oob: no reply before the bearer timeout")
	ErrUnknownCmd = errors.New("oob: unknown command")
)

// Pair stores a management key for a bridge. localRole is the Hub's role:
// RoleImporter when the key came from the kit's bundle (the kit issued it),
// RoleIssuer when the Hub generated it. Returns the derived peer id.
func (s *Service) Pair(ctx context.Context, tenantID, bridgeID string, key []byte, localRole Role, phone, satIMEI string) (uint16, error) {
	if len(key) != KeyLen {
		return 0, ErrBadKey
	}
	enc, err := crypto.Encrypt(s.masterKey, key)
	if err != nil {
		return 0, fmt.Errorf("oob: encrypt key: %w", err)
	}
	peerID := PeerIDFromKey(key)
	p := &store.OOBPeer{TenantID: tenantID, BridgeID: bridgeID, PeerID: int(peerID), KeyEnc: enc, LocalRole: int(localRole),
		Phone: strings.TrimSpace(phone), SatIMEI: strings.TrimSpace(satIMEI), Enabled: true}
	if old, err := s.store.GetOOBPeer(ctx, tenantID, bridgeID); err == nil && old != nil {
		// Re-pairing with the same key keeps the counters; a new key restarts them.
		if string(old.KeyEnc) != "" {
			if oldKey, err := crypto.Decrypt(s.masterKey, old.KeyEnc); err == nil && string(oldKey) == string(key) {
				p.TxCounter, p.RxHigh, p.RxWindow, p.CreatedAt = old.TxCounter, old.RxHigh, old.RxWindow, old.CreatedAt
			}
		}
		if p.Phone == "" {
			p.Phone = old.Phone
		}
		if p.SatIMEI == "" {
			p.SatIMEI = old.SatIMEI
		}
	}
	if err := s.store.UpsertOOBPeer(ctx, p); err != nil {
		return 0, err
	}
	s.log(ctx, tenantID, "oob_paired", "system", fmt.Sprintf("bridge=%s peer_id=%d role=%d phone=%s sat=%s", bridgeID, peerID, localRole, p.Phone, p.SatIMEI))
	return peerID, nil
}

// Unpair removes the pairing.
func (s *Service) Unpair(ctx context.Context, tenantID, bridgeID string) error {
	if err := s.store.DeleteOOBPeer(ctx, tenantID, bridgeID); err != nil {
		return err
	}
	s.log(ctx, tenantID, "oob_unpaired", "system", "bridge="+bridgeID)
	return nil
}

// Peer returns the pairing (nil when none).
func (s *Service) Peer(ctx context.Context, tenantID, bridgeID string) (*store.OOBPeer, error) {
	p, err := s.store.GetOOBPeer(ctx, tenantID, bridgeID)
	if err != nil {
		return nil, nil //nolint:nilerr // not paired
	}
	return p, nil
}

func (s *Service) key(p *store.OOBPeer) ([]byte, error) {
	k, err := crypto.Decrypt(s.masterKey, p.KeyEnc)
	if err != nil {
		return nil, fmt.Errorf("oob: decrypt key: %w", err)
	}
	if len(k) != KeyLen {
		return nil, ErrBadKey
	}
	return k, nil
}

// ChooseBearer picks the bearer for a bridge: the requested one when set,
// else SMS when the kit has a phone number, else a satellite bearer when it
// has a modem (imt for IMT modems, sbd otherwise).
func (s *Service) ChooseBearer(p *store.OOBPeer, via string, isIMT func(imei string) bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	has := func(b string) bool { _, ok := s.transports[b]; return ok }
	via = strings.ToLower(strings.TrimSpace(via))
	if via != "" && via != BearerMQTT {
		if !has(via) {
			return "", fmt.Errorf("oob: bearer %q is not available", via)
		}
		switch via {
		case BearerSMS:
			if p.Phone == "" {
				return "", errors.New("oob: bridge has no phone number for SMS")
			}
		case BearerIMT, BearerSBD:
			if p.SatIMEI == "" {
				return "", errors.New("oob: bridge has no satellite IMEI")
			}
		}
		return via, nil
	}
	if p.Phone != "" && has(BearerSMS) {
		return BearerSMS, nil
	}
	if p.SatIMEI != "" {
		if isIMT != nil && isIMT(p.SatIMEI) && has(BearerIMT) {
			return BearerIMT, nil
		}
		if has(BearerSBD) {
			return BearerSBD, nil
		}
		if has(BearerIMT) {
			return BearerIMT, nil
		}
	}
	return "", ErrNoBearer
}

func (s *Service) allow(bridgeID, bearer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bridgeID + "|" + bearer
	cut := s.now().Add(-time.Hour)
	kept := s.sent[key][:0]
	for _, t := range s.sent[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= s.maxPerHour {
		s.sent[key] = kept
		return false
	}
	s.sent[key] = append(kept, s.now())
	return true
}

func pendingKey(bridgeID string, counter uint32) string {
	return fmt.Sprintf("%s|%d", bridgeID, counter&0xFFFF)
}

// Send seals one command for the bridge, delivers it over bearer and waits
// for the reply (or the bearer's timeout). ctx may carry a shorter deadline.
func (s *Service) Send(ctx context.Context, tenantID, bridgeID, bearer, cmdName string, args ArgSpec, noReply bool) (*Reply, error) {
	cmd, ok := CommandByName(cmdName)
	if !ok {
		return nil, ErrUnknownCmd
	}
	wireArgs, err := BuildArgs(cmd.Code, args)
	if err != nil {
		return nil, err
	}
	p, err := s.store.GetOOBPeer(ctx, tenantID, bridgeID)
	if err != nil || p == nil || !p.Enabled {
		return nil, ErrNotPaired
	}
	s.mu.Lock()
	t, ok := s.transports[bearer]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("oob: bearer %q is not available", bearer)
	}
	if !s.allow(bridgeID, bearer) {
		return nil, ErrRateLimit
	}
	key, err := s.key(p)
	if err != nil {
		return nil, err
	}
	// The counter is persisted before the frame leaves (nonce uniqueness).
	n, err := s.store.NextOOBCounter(ctx, tenantID, bridgeID)
	if err != nil {
		return nil, fmt.Errorf("oob: counter: %w", err)
	}
	if n <= 0 || n >= 0xFFFFFFFF {
		return nil, errors.New("oob: key exhausted, re-pair the bridge")
	}
	counter := uint32(n)
	f := Frame{Enc: s.encrypt, NoReply: noReply, PeerID: uint16(p.PeerID), Counter: counter, Cmd: cmd.Code, Args: wireArgs}
	wire, err := Seal(f, key, Role(p.LocalRole))
	if err != nil {
		return nil, err
	}
	text := Encode(wire)

	var ch chan Reply
	if !noReply {
		ch = make(chan Reply, 4)
		s.mu.Lock()
		s.pending[pendingKey(bridgeID, counter)] = ch
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.pending, pendingKey(bridgeID, counter))
			s.mu.Unlock()
		}()
	}
	if err := t.Send(ctx, tenantID, p, text); err != nil {
		s.log(ctx, tenantID, "oob_send_failed", "system", fmt.Sprintf("bridge=%s bearer=%s cmd=%s counter=%d error=%s", bridgeID, bearer, cmd.Name, counter, err))
		return nil, fmt.Errorf("oob: send over %s: %w", bearer, err)
	}
	s.log(ctx, tenantID, "oob_command_sent", "system", fmt.Sprintf("bridge=%s bearer=%s cmd=%s counter=%d enc=%v", bridgeID, bearer, cmd.Name, counter, s.encrypt))
	slog.Info("oob: command sent", "bridge", bridgeID, "bearer", bearer, "cmd", cmd.Name, "counter", counter)
	if noReply {
		return &Reply{Bearer: bearer, Counter: counter, Result: "sent"}, nil
	}
	timeout := t.Timeout()
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < timeout {
		timeout = time.Until(dl)
	}
	select {
	case r := <-ch:
		return &r, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("%w (%s, %s)", ErrTimeout, bearer, timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HandleInbound classifies text that arrived over bearer from origin (the
// kit's phone number or IMEI). It returns true when the text carried a
// management frame, whether or not it was accepted, so the caller keeps it
// out of the message pipeline. Unauthenticated, replayed or unknown-peer
// frames are dropped silently (spec section 5).
func (s *Service) HandleInbound(ctx context.Context, bearer, origin, text string) bool {
	wire, ok := ExtractFrame(text)
	if !ok {
		return false
	}
	h, err := ParseHeader(wire)
	if err != nil {
		return false
	}
	peers, err := s.store.ListOOBPeersByPeerID(ctx, int(h.PeerID))
	if err != nil || len(peers) == 0 {
		slog.Warn("oob: frame from unknown peer dropped", "peer_id", h.PeerID, "bearer", bearer, "origin", origin)
		return true
	}
	for i := range peers {
		p := &peers[i]
		key, err := s.key(p)
		if err != nil {
			continue
		}
		f, err := Open(wire, key, Role(p.LocalRole).Other())
		if err != nil {
			continue
		}
		var w Window
		w.Load(uint32(p.RxHigh), uint64(p.RxWindow))
		if !w.Accept(f.Counter) {
			slog.Warn("oob: replayed frame dropped", "bridge", p.BridgeID, "counter", f.Counter, "bearer", bearer)
			s.log(ctx, p.TenantID, "oob_replay_dropped", origin, fmt.Sprintf("bridge=%s bearer=%s counter=%d", p.BridgeID, bearer, f.Counter))
			return true
		}
		high, bits := w.State()
		if err := s.store.SetOOBReplayWindow(ctx, p.TenantID, p.BridgeID, int64(high), int64(bits)); err != nil {
			slog.Warn("oob: persist replay window", "error", err)
		}
		if !f.Reply {
			// Kits do not command the Hub; a request is recorded and ignored.
			s.log(ctx, p.TenantID, "oob_request_ignored", origin, fmt.Sprintf("bridge=%s bearer=%s cmd=0x%02x counter=%d", p.BridgeID, bearer, f.Cmd, f.Counter))
			return true
		}
		ra, err := ParseReplyArgs(f.Args)
		if err != nil {
			slog.Warn("oob: reply args unparsable", "bridge", p.BridgeID, "error", err)
			return true
		}
		r := Reply{Bearer: bearer, RC: ra.RC, Result: ra.RC.String(), Body: string(ra.Body), Counter: uint32(ra.ReqCounterLo), Seq: ra.Seq, Total: ra.Total, Received: s.now().UTC()}
		s.log(ctx, p.TenantID, "oob_reply", origin, fmt.Sprintf("bridge=%s bearer=%s req_counter=%d rc=%s seq=%d/%d body=%q", p.BridgeID, bearer, ra.ReqCounterLo, ra.RC, ra.Seq, ra.Total, ra.Body))
		slog.Info("oob: reply", "bridge", p.BridgeID, "bearer", bearer, "rc", ra.RC.String(), "req_counter", ra.ReqCounterLo, "body", string(ra.Body))
		s.mu.Lock()
		ch := s.pending[pendingKey(p.BridgeID, uint32(ra.ReqCounterLo))]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- r:
			default:
			}
		}
		return true
	}
	slog.Warn("oob: frame failed authentication for every candidate peer", "peer_id", h.PeerID, "bearer", bearer, "origin", origin)
	return true
}

func (s *Service) log(ctx context.Context, tenantID, action, actor, detail string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Log(ctx, tenantID, action, actor, detail, ""); err != nil {
		slog.Warn("audit: oob", "action", action, "error", err)
	}
}

// KeyHex renders a key for the pairing response (shown once).
func KeyHex(key []byte) string { return hex.EncodeToString(key) }

// ParseKeyHex parses a 64-hex-character key.
func ParseKeyHex(s string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != KeyLen {
		return nil, ErrBadKey
	}
	return k, nil
}
