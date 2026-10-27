// Package lxmfhub gives every tenant an LXMF address on the Hub's Reticulum
// node (MESHSAT-1555): its own lxmf.delivery identity, announced with a
// display name and stamp cost; an inbox; and outbound messages through a
// per-tenant queue whose states come from delivery proofs. The identities
// live in the tenant's encrypted provider row ("lxmf"); the destinations
// live on the leader's node, registered for each term and rebuilt when a
// row changes on any replica.
package lxmfhub

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Store is the slice of store.Store the service uses.
type Store interface {
	InsertLXMFMessage(ctx context.Context, tenantID string, m *store.LXMFMessage) error
	ListLXMFMessages(ctx context.Context, tenantID string, q store.LXMFQuery) ([]store.LXMFMessage, error)
	UpdateLXMFMessageState(ctx context.Context, tenantID, id string, upd store.LXMFStateUpdate) error
	CountLXMFMessages(ctx context.Context, tenantID string) (int, error)
	PruneLXMFMessages(ctx context.Context, tenantID string, keep int) (int, error)
	InsertLXMFHeld(ctx context.Context, tenantID string, m *store.LXMFHeldMessage) error
	ListLXMFHeldFor(ctx context.Context, tenantID, destination string) ([]store.LXMFHeldMessage, error)
	GetLXMFHeld(ctx context.Context, tenantID, id string) (*store.LXMFHeldMessage, error)
	DeleteLXMFHeld(ctx context.Context, tenantID, destination string, ids []string) (int, error)
	LXMFHeldBytes(ctx context.Context, tenantID string) (int64, error)
	PruneLXMFHeld(ctx context.Context, tenantID string, cutoff time.Time, maxBytes int64) (int, error)
	ClaimOnce(ctx context.Context, key string) (bool, error)
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
}

// Accounts is the slice of the integrations service the identities live in.
type Accounts interface {
	ForTenant(ctx context.Context, tenantID, provider string) (*integrations.Account, error)
	Set(ctx context.Context, tenantID, provider string, fields map[string]string) (*integrations.Account, error)
}

// Nodes gives a Private tenant its own node (rnsnode.Service, MESHSAT-1722):
// created on first use in the term, keyed by the tenant's network name and
// passphrase; a changed key closes the connections that used the old one.
type Nodes interface {
	PrivateNode(tenantID, netname, passphrase string) (*rns.Node, error)
	ReleasePrivate(tenantID string)
	// ForgetPaths drops the main node's cached paths to an address that is
	// not on it, so no cache answers for a Private or Off address there.
	ForgetPaths(hashes ...[rns.HashLen]byte)
}

// Auditor records an event on the tenant's audit chain.
type Auditor func(ctx context.Context, tenantID, action, actor, detail string)

// Config bounds the service; zero values take the defaults below.
type Config struct {
	// AnnounceInterval is how often each enabled address is announced.
	AnnounceInterval time.Duration
	// InboundPerMinute caps what one address accepts; beyond it messages
	// are dropped and counted.
	InboundPerMinute int
	// InboxMax is the rows kept per tenant; the oldest go first, never one
	// still being sent.
	InboxMax int
	// SendTimeout bounds one delivery attempt (the proof must arrive in it).
	SendTimeout time.Duration
	// MaxConcurrentSends bounds deliveries in flight on the node.
	MaxConcurrentSends int
	// PropagationMaxBytes caps what one tenant's propagation node holds.
	PropagationMaxBytes int64
	Now                 func() time.Time
}

const (
	defaultAnnounceInterval = 30 * time.Minute
	defaultInboundPerMinute = 60
	defaultInboxMax         = 10000
	defaultSendTimeout      = 3 * time.Minute
	defaultConcurrentSends  = 8
	firstAnnounceDelay      = 5 * time.Second
	senderTick              = 2 * time.Second
	// MaxContentBytes bounds an outbound message (LXMF carries far more as a
	// resource, but a text inbox is not a file transfer).
	MaxContentBytes = 64 * 1024
	MaxTitleBytes   = 256
)

var hexAddress = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Service runs the tenants' LXMF endpoints on the leader's node.
type Service struct {
	cfg      Config
	store    Store
	accounts Accounts
	audit    Auditor
	now      func() time.Time

	mu      sync.Mutex
	node    *rns.Node  // the main node: Public addresses
	nodes   Nodes      // Private tenants' own nodes; nil registers no Private address
	kits    *kitAccess // the kits' joins (MESHSAT-1715); nil tells no kit anything
	routers map[string]*tenantRouter
	cancel  context.CancelFunc
	kick    chan struct{}
	sem     chan struct{}

	// tl serializes register for one tenant: Reload runs on a goroutine per
	// row change, and two interleaving could leave an Off address live on
	// the main node or register onto a node another call just released.
	tlMu sync.Mutex
	tl   map[string]*sync.Mutex

	received, dropped, delivered, failed atomic.Uint64
	heldIn, heldServed                   atomic.Uint64
}

type tenantRouter struct {
	id *Identity
	// node is where the address lives: the main node (Public) or the
	// tenant's own (Private). Everything the router does goes through it.
	node         *rns.Node
	router       *lxmf.Router
	pn           *rns.Destination // the propagation node, nil when disabled
	registeredAt time.Time
	lastAnnounce time.Time
	joinAnnounce time.Time // the last announce a joining connection caused
	joinPending  bool      // a connection joined since; announce when the minute is up
	bucket       *bucket
}

// New builds the service; it does nothing until OnNode.
func New(cfg Config, st Store, accounts Accounts, audit Auditor) *Service {
	if cfg.AnnounceInterval <= 0 {
		cfg.AnnounceInterval = defaultAnnounceInterval
	}
	if cfg.InboundPerMinute <= 0 {
		cfg.InboundPerMinute = defaultInboundPerMinute
	}
	if cfg.InboxMax <= 0 {
		cfg.InboxMax = defaultInboxMax
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = defaultSendTimeout
	}
	if cfg.MaxConcurrentSends <= 0 {
		cfg.MaxConcurrentSends = defaultConcurrentSends
	}
	if cfg.PropagationMaxBytes <= 0 {
		cfg.PropagationMaxBytes = defaultPropagationMaxBytes
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, store: st, accounts: accounts, audit: audit, now: now, routers: map[string]*tenantRouter{},
		kick: make(chan struct{}, 1), sem: make(chan struct{}, cfg.MaxConcurrentSends), tl: map[string]*sync.Mutex{}}
}

// SetNodes attaches the Private tenants' nodes; until then a Private
// address is registered nowhere, which is the closed end.
func (s *Service) SetNodes(n Nodes) {
	s.mu.Lock()
	s.nodes = n
	s.mu.Unlock()
}

func (s *Service) auditLog(ctx context.Context, tenantID, action, actor, detail string) {
	if s.audit != nil {
		s.audit(ctx, tenantID, action, actor, detail)
	}
}

// --- the node's term ---

// OnNode is rnsnode.Config.OnNode: the leader's node for this term. Every
// tenant with an enabled identity gets its delivery destination, and the
// announcer and sender start.
func (s *Service) OnNode(n *rns.Node) {
	s.mu.Lock()
	s.node = n
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	tenants, err := s.store.ListTenants(ctx)
	if err != nil {
		slog.Error("lxmf: could not list tenants, no address registered", "error", err)
	}
	n_ := 0
	for _, t := range tenants {
		if t.Status == store.TenantDeleted {
			continue
		}
		if s.register(ctx, t.ID) {
			n_++
		}
	}
	slog.Info("lxmf: tenant addresses registered on the node", "addresses", n_)
	go s.announcer(ctx)
	go s.sender(ctx)
}

// OnNodeStopped is rnsnode.Config.OnNodeStopped: the term is over.
func (s *Service) OnNodeStopped() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.node = nil
	s.routers = map[string]*tenantRouter{}
	s.mu.Unlock()
}

// Active is true on the replica whose node carries the addresses.
func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.node != nil
}

// register (re)builds one tenant's router from its row on the node its
// access mode names; false when the tenant has no address that is live.
func (s *Service) register(ctx context.Context, tenantID string) bool {
	id, err := s.LoadIdentity(ctx, tenantID)
	if err != nil {
		if !errors.Is(err, ErrNoIdentity) {
			slog.Warn("lxmf: identity unreadable, address not registered", "tenant", tenantID, "error", err)
		}
		return false
	}
	if !s.Active() {
		return false
	}
	if needsAccessCode(id) {
		// An address made before MESHSAT-1722 is Private by default and has
		// no code yet: the leader makes one, once. Outside the tenant's
		// lock: the write's own reload may come back into register.
		if _, err := s.fillAccessCode(ctx, tenantID); err != nil {
			slog.Warn("lxmf: a Private address has no access code yet, not registered", "tenant", tenantID, "error", err)
		}
	}
	lk := s.tenantLock(tenantID)
	lk.Lock()
	defer lk.Unlock()
	// What is stored now is what gets registered, read under the lock.
	if id, err = s.LoadIdentity(ctx, tenantID); err != nil || !s.Active() {
		return false
	}
	var node *rns.Node
	switch {
	case !id.Enabled || id.AccessMode == AccessOff:
	case needsAccessCode(id):
	case id.AccessMode == AccessPublic:
		s.mu.Lock()
		node = s.node
		s.mu.Unlock()
	default:
		s.mu.Lock()
		nodes := s.nodes
		s.mu.Unlock()
		if nodes == nil {
			break
		}
		// Never under s.mu: the tenant's node may close connections, and
		// their callbacks run inside rnsnode.
		if node, err = nodes.PrivateNode(tenantID, id.IFACNetname, id.IFACPassphrase); err != nil {
			slog.Warn("lxmf: the tenant's private node did not start, address not registered", "tenant", tenantID, "error", err)
			node = nil
		}
	}
	s.mu.Lock()
	main, nodes := s.node, s.nodes
	s.mu.Unlock()
	if node != main && nodes != nil {
		nodes.ForgetPaths(h16hex(id.Address), h16hex(id.PropagationAddress))
	}
	if node == nil {
		s.dropLocked(tenantID)
		return false
	}
	s.mu.Lock()
	if s.node == nil {
		s.mu.Unlock()
		return false
	}
	prev := s.routers[tenantID]
	s.unregisterLocked(tenantID)
	tid := tenantID
	r := lxmf.New(node, lxmf.Config{Identity: id.ident, DisplayName: id.DisplayName, StampCost: id.StampCost,
		EnforceStamps: id.StampCost > 0, OnMessage: func(m *lxmf.Message) { s.onMessage(tid, m) }})
	tr := &tenantRouter{id: id, node: node, router: r, registeredAt: s.now(), bucket: newBucket(s.cfg.InboundPerMinute, s.now())}
	if id.PropagationEnabled {
		s.registerPNLocked(tr)
	}
	s.routers[tenantID] = tr
	s.mu.Unlock()
	if prev != nil && prev.id.AccessMode == AccessPrivate && id.AccessMode != AccessPrivate && nodes != nil {
		nodes.ReleasePrivate(tenantID)
	}
	slog.Info("lxmf: address registered", "tenant", tenantID, "address", id.Address, "access_mode", id.AccessMode,
		"display_name", id.DisplayName, "stamp_cost", id.StampCost)
	return true
}

func h16hex(s string) [rns.HashLen]byte {
	var out [rns.HashLen]byte
	b, _ := hex.DecodeString(s)
	copy(out[:], b)
	return out
}

func needsAccessCode(id *Identity) bool {
	return id.Enabled && id.AccessMode == AccessPrivate && (id.IFACNetname == "" || id.IFACPassphrase == "")
}

func (s *Service) tenantLock(tenantID string) *sync.Mutex {
	s.tlMu.Lock()
	defer s.tlMu.Unlock()
	l := s.tl[tenantID]
	if l == nil {
		l = &sync.Mutex{}
		s.tl[tenantID] = l
	}
	return l
}

// dropLocked unregisters a tenant's address and stops its own node, if
// any; the caller holds the tenant's lock.
func (s *Service) dropLocked(tenantID string) {
	s.mu.Lock()
	had := s.routers[tenantID] != nil
	s.unregisterLocked(tenantID)
	nodes := s.nodes
	s.mu.Unlock()
	if nodes != nil {
		nodes.ReleasePrivate(tenantID)
	}
	if had {
		slog.Info("lxmf: address unregistered", "tenant", tenantID)
	}
}

// fillAccessCode gives a Private address that has none its network name and
// passphrase. One replica writes (a claim per tenant and minute); the row's
// change reaches every replica.
func (s *Service) fillAccessCode(ctx context.Context, tenantID string) (*Identity, error) {
	won, err := s.store.ClaimOnce(ctx, fmt.Sprintf("lxmf-ifac:%s:%d", tenantID, s.now().Unix()/60))
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, errors.New("another replica is writing it")
	}
	netname, pass, err := newAccessCode()
	if err != nil {
		return nil, err
	}
	if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderLXMF, map[string]string{"ifac_netname": netname, "ifac_passphrase": pass}); err != nil {
		return nil, err
	}
	s.auditLog(ctx, tenantID, "lxmf_access_code_created", "hub", "network name "+netname+" for the Private access mode")
	id, err := s.LoadIdentity(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if id.IFACNetname == "" || id.IFACPassphrase == "" {
		return nil, errors.New("the access code was not stored")
	}
	return id, nil
}

func (s *Service) unregisterLocked(tenantID string) {
	if tr := s.routers[tenantID]; tr != nil {
		tr.node.RemoveDestination(tr.router.Hash())
		if tr.pn != nil {
			tr.node.RemoveDestination(tr.pn.Hash)
		}
		delete(s.routers, tenantID)
	}
}

// Reload is called on every replica when a tenant's provider accounts
// changed (integrations.ReloadTopic); the leader rebuilds the address on the
// node its access mode names and, when it is live, announces it with its new
// name and cost at once, then tells the tenant's kits what the row says now.
func (s *Service) Reload(tenantID string) {
	if !s.Active() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.register(ctx, tenantID) {
		s.announceOne(tenantID)
	}
	s.pushKitAccess(ctx, tenantID)
}

// --- announcing ---

// announcerTick is how often the announcer looks for due addresses.
var announcerTick = 10 * time.Second

func (s *Service) announcer(ctx context.Context) {
	t := time.NewTicker(announcerTick)
	defer t.Stop()
	lastSweep := s.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.now().Sub(lastSweep) >= pnSweepEvery {
			lastSweep = s.now()
			s.sweepHeld(ctx)
		}
		now := s.now()
		s.mu.Lock()
		var due []string
		for tid, tr := range s.routers {
			first := tr.lastAnnounce.IsZero() && now.Sub(tr.registeredAt) >= firstAnnounceDelay
			if first || (!tr.lastAnnounce.IsZero() && now.Sub(tr.lastAnnounce) >= s.cfg.AnnounceInterval) {
				due = append(due, tid)
			}
		}
		s.mu.Unlock()
		for _, tid := range due {
			s.announceOne(tid)
		}
		s.flushJoins()
	}
}

// joinAnnounceEvery bounds the announces joining connections cause: one per
// address a minute, however many connect. A join inside the minute is
// announced when the minute is up, so every joiner hears within a minute.
const joinAnnounceEvery = time.Minute

// OnJoin is rnsnode.Config.OnJoin: a connection joined the main node (tenant
// "") or a Private tenant's node. A node announces nothing to a newcomer, and
// the addresses are announced every AnnounceInterval, so a client that had not
// asked for a path did not list the address (MeshChat's and CrossTalk's peers,
// NomadNet's announce stream) for up to 30 minutes after it connected
// (MESHSAT-1561). The addresses on that node are announced again, at most
// once a minute each.
func (s *Service) OnJoin(tenantID string) {
	now := s.now()
	s.mu.Lock()
	main := s.node
	var due []string
	for tid, tr := range s.routers {
		onIt := tr.node != nil && (tenantID == "" && tr.node == main || tenantID != "" && tid == tenantID && tr.node != main)
		switch {
		case !onIt:
		case now.Sub(tr.joinAnnounce) >= joinAnnounceEvery:
			tr.joinAnnounce, tr.joinPending = now, false
			due = append(due, tid)
		default:
			tr.joinPending = true
		}
	}
	s.mu.Unlock()
	for _, tid := range due {
		s.announceOne(tid)
	}
}

// flushJoins announces the addresses whose joiners waited for the minute.
func (s *Service) flushJoins() {
	now := s.now()
	s.mu.Lock()
	var due []string
	for tid, tr := range s.routers {
		if tr.joinPending && now.Sub(tr.joinAnnounce) >= joinAnnounceEvery {
			tr.joinAnnounce, tr.joinPending = now, false
			due = append(due, tid)
		}
	}
	s.mu.Unlock()
	for _, tid := range due {
		s.announceOne(tid)
	}
}

func (s *Service) announceOne(tenantID string) {
	s.mu.Lock()
	tr := s.routers[tenantID]
	if tr != nil {
		tr.lastAnnounce = s.now()
	}
	s.mu.Unlock()
	if tr == nil {
		return
	}
	if err := tr.router.Announce(); err != nil {
		slog.Debug("lxmf: announce failed", "tenant", tenantID, "error", err)
	}
	if pn, node := tr.pn, tr.node; pn != nil && node != nil {
		if err := node.Announce(pn, "", false); err != nil {
			slog.Debug("lxmf: propagation announce failed", "tenant", tenantID, "error", err)
		}
	}
}

// --- inbound ---

func (s *Service) onMessage(tenantID string, m *lxmf.Message) {
	now := s.now()
	s.mu.Lock()
	tr := s.routers[tenantID]
	allowed := tr != nil && tr.bucket.allow(now)
	s.mu.Unlock()
	if tr == nil {
		return
	}
	hash := hex.EncodeToString(m.Hash[:])
	if !allowed {
		s.dropped.Add(1)
		slog.Warn("lxmf: inbound rate limit, message dropped", "tenant", tenantID, "from", hex.EncodeToString(m.Source[:]), "hash", hash)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	won, err := s.store.ClaimOnce(ctx, "lxmf-in:"+tenantID+":"+hash)
	if err != nil || !won {
		return
	}
	row := &store.LXMFMessage{Hash: hash, Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived,
		Source: hex.EncodeToString(m.Source[:]), Destination: hex.EncodeToString(m.Dest[:]),
		Title: clipUTF8(string(m.Title), MaxTitleBytes), Content: clipUTF8(string(m.Content), MaxContentBytes), FieldsJSON: fieldsJSON(m.Fields),
		Method: m.Method.String(), StampValue: m.StampValue, SignatureValid: m.SignatureValid,
		MessageAt: time.Unix(int64(m.Timestamp), 0).UTC()}
	if err := s.store.InsertLXMFMessage(ctx, tenantID, row); err != nil && !errors.Is(err, store.ErrDuplicate) {
		slog.Error("lxmf: inbox write failed", "tenant", tenantID, "hash", hash, "error", err)
		return
	}
	s.received.Add(1)
	metrics.MessageThroughput.WithLabelValues("in", "lxmf").Inc()
	if n, err := s.store.CountLXMFMessages(ctx, tenantID); err == nil && n > s.cfg.InboxMax {
		if gone, err := s.store.PruneLXMFMessages(ctx, tenantID, s.cfg.InboxMax); err == nil && gone > 0 {
			slog.Info("lxmf: inbox pruned", "tenant", tenantID, "removed", gone)
		}
	}
}

func clipUTF8(s string, max int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// --- outbound ---

// SendRequest is one message an operator wants delivered.
type SendRequest struct {
	Destination string // 32 hex characters, the peer's lxmf.delivery hash
	Content     string
	Title       string
	// Method: "" or "auto" lets the size decide, "direct" or "opportunistic" forces one.
	Method string
	Actor  string
}

// ParseMethod maps the API's method names.
func ParseMethod(s string) (lxmf.Method, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return 0, nil
	case "direct":
		return lxmf.Direct, nil
	case "opportunistic":
		return lxmf.Opportunistic, nil
	}
	return 0, errors.New("lxmf: method must be auto, direct or opportunistic")
}

// Send validates and queues one message; the leader's sender delivers it.
// The row is the receipt the API answers with.
func (s *Service) Send(ctx context.Context, tenantID string, req SendRequest) (*store.LXMFMessage, error) {
	id, err := s.LoadIdentity(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if !id.Enabled {
		return nil, errors.New("lxmf: this tenant's address is disabled")
	}
	dest := strings.ToLower(strings.TrimSpace(req.Destination))
	if !hexAddress.MatchString(dest) {
		return nil, errors.New("lxmf: destination must be the peer's 32-character LXMF address")
	}
	if dest == id.Address {
		return nil, errors.New("lxmf: destination is this tenant's own address")
	}
	if strings.TrimSpace(req.Content) == "" || len(req.Content) > MaxContentBytes {
		return nil, fmt.Errorf("lxmf: content must be 1 to %d bytes", MaxContentBytes)
	}
	if len(req.Title) > MaxTitleBytes {
		return nil, fmt.Errorf("lxmf: title must be at most %d bytes", MaxTitleBytes)
	}
	method, err := ParseMethod(req.Method)
	if err != nil {
		return nil, err
	}
	row := &store.LXMFMessage{Direction: store.LXMFDirectionOut, State: store.LXMFStateQueued, Source: id.Address, Destination: dest,
		Title: req.Title, Content: req.Content, CreatedByEmail: req.Actor, MessageAt: s.now()}
	if method != 0 {
		row.Method = method.String()
	}
	if err := s.store.InsertLXMFMessage(ctx, tenantID, row); err != nil {
		return nil, err
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return row, nil
}

// sender drains every registered tenant's queue on this node.
func (s *Service) sender(ctx context.Context) {
	t := time.NewTicker(senderTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		s.mu.Lock()
		tenants := make([]string, 0, len(s.routers))
		for tid := range s.routers {
			tenants = append(tenants, tid)
		}
		s.mu.Unlock()
		for _, tid := range tenants {
			s.drain(ctx, tid)
		}
	}
}

func (s *Service) drain(ctx context.Context, tenantID string) {
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	queued, err := s.store.ListLXMFMessages(lctx, tenantID, store.LXMFQuery{State: store.LXMFStateQueued, Limit: 20})
	if err == nil {
		// A send interrupted by a failover or crash stays "sending" for good
		// otherwise.
		stale, _ := s.store.ListLXMFMessages(lctx, tenantID, store.LXMFQuery{State: store.LXMFStateSending, Limit: 20, Before: s.now().Add(-2 * s.cfg.SendTimeout)})
		for _, m := range stale {
			_ = s.store.UpdateLXMFMessageState(lctx, tenantID, m.ID, store.LXMFStateUpdate{State: store.LXMFStateFailed, Detail: "send interrupted"})
			s.failed.Add(1)
		}
	}
	cancel()
	if err != nil {
		slog.Warn("lxmf: queue read failed", "tenant", tenantID, "error", err)
		return
	}
	for i := range queued {
		m := queued[i]
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func() {
			defer func() { <-s.sem }()
			s.deliver(ctx, tenantID, &m)
		}()
	}
}

func (s *Service) deliver(ctx context.Context, tenantID string, m *store.LXMFMessage) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	won, err := s.store.ClaimOnce(cctx, "lxmf-send:"+tenantID+":"+m.ID)
	if err == nil && won {
		err = s.store.UpdateLXMFMessageState(cctx, tenantID, m.ID, store.LXMFStateUpdate{State: store.LXMFStateSending})
	}
	cancel()
	if err != nil || !won {
		return
	}
	s.mu.Lock()
	tr := s.routers[tenantID]
	s.mu.Unlock()
	if tr == nil {
		s.finish(tenantID, m.ID, store.LXMFStateUpdate{State: store.LXMFStateFailed, Detail: "address no longer registered"})
		return
	}
	method, _ := ParseMethod(m.Method)
	destBytes, _ := hex.DecodeString(m.Destination)
	var dest [lxmf.DestLen]byte
	copy(dest[:], destBytes)
	sctx, scancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	defer scancel()
	msg, res, serr := tr.router.Send(sctx, dest, []byte(m.Content), []byte(m.Title), lxmf.MapOf(), method)
	upd := store.LXMFStateUpdate{State: store.LXMFStateDelivered}
	if msg != nil {
		upd.Hash = hex.EncodeToString(msg.Hash[:])
	}
	if res != nil && res.Method != 0 {
		upd.Method = res.Method.String()
	}
	if serr != nil {
		upd.State, upd.Detail = store.LXMFStateFailed, serr.Error()
		s.failed.Add(1)
		slog.Info("lxmf: delivery failed", "tenant", tenantID, "id", m.ID, "dest", m.Destination, "error", serr)
	} else {
		s.delivered.Add(1)
		metrics.MessageThroughput.WithLabelValues("out", "lxmf").Inc()
		slog.Info("lxmf: delivered", "tenant", tenantID, "id", m.ID, "dest", m.Destination, "method", upd.Method, "rtt", res.RTT)
	}
	s.finish(tenantID, m.ID, upd)
}

func (s *Service) finish(tenantID, id string, upd store.LXMFStateUpdate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.UpdateLXMFMessageState(ctx, tenantID, id, upd); err != nil {
		slog.Error("lxmf: state write failed", "tenant", tenantID, "id", id, "state", upd.State, "error", err)
	}
}

// --- views ---

// Peers lists the LXMF peers this tenant's address has seen, on the leader;
// empty elsewhere.
func (s *Service) Peers(tenantID string) []lxmf.Peer {
	s.mu.Lock()
	tr := s.routers[tenantID]
	s.mu.Unlock()
	if tr == nil {
		return []lxmf.Peer{}
	}
	return tr.router.Peers()
}

// RegisteredOnThisReplica reports whether the tenant's address is live on
// this replica's node, with its last announce time.
func (s *Service) RegisteredOnThisReplica(tenantID string) (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tr := s.routers[tenantID]
	if tr == nil {
		return false, time.Time{}
	}
	return true, tr.lastAnnounce
}

// Stats are this replica's counters.
type Stats struct {
	Addresses  int    `json:"addresses"`
	Received   uint64 `json:"received"`
	Dropped    uint64 `json:"dropped"`
	Delivered  uint64 `json:"delivered"`
	Failed     uint64 `json:"failed"`
	HeldIn     uint64 `json:"held_in"`
	HeldServed uint64 `json:"held_served"`
}

// Stats returns this replica's counters.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	n := len(s.routers)
	s.mu.Unlock()
	return Stats{Addresses: n, Received: s.received.Load(), Dropped: s.dropped.Load(), Delivered: s.delivered.Load(), Failed: s.failed.Load(),
		HeldIn: s.heldIn.Load(), HeldServed: s.heldServed.Load()}
}
