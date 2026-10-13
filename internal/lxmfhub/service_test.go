package lxmfhub

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnsnode"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- fakes ---

type memStore struct {
	mu      sync.Mutex
	rows    map[string]map[string]*store.LXMFMessage // tenant -> id -> row
	claims  map[string]bool
	tenants []store.Tenant
	seq     int
}

func newMemStore(tenants ...string) *memStore {
	m := &memStore{rows: map[string]map[string]*store.LXMFMessage{}, claims: map[string]bool{}}
	for _, t := range tenants {
		m.tenants = append(m.tenants, store.Tenant{ID: t, Name: "Tenant " + t, Status: store.TenantActive})
	}
	return m
}

func (m *memStore) InsertLXMFMessage(_ context.Context, tenantID string, row *store.LXMFMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row.ID == "" {
		m.seq++
		row.ID = strings.Repeat("0", 3) + string(rune('a'+m.seq))
	}
	row.TenantID = tenantID
	if m.rows[tenantID] == nil {
		m.rows[tenantID] = map[string]*store.LXMFMessage{}
	}
	if _, dup := m.rows[tenantID][row.ID]; dup {
		return store.ErrDuplicate
	}
	cp := *row
	cp.CreatedAt = time.Now()
	m.rows[tenantID][row.ID] = &cp
	return nil
}

func (m *memStore) ListLXMFMessages(_ context.Context, tenantID string, q store.LXMFQuery) ([]store.LXMFMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.LXMFMessage
	for _, r := range m.rows[tenantID] {
		if (q.Direction == "" || r.Direction == q.Direction) && (q.State == "" || r.State == q.State) && (q.Before.IsZero() || r.CreatedAt.Before(q.Before)) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (m *memStore) UpdateLXMFMessageState(_ context.Context, tenantID, id string, upd store.LXMFStateUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[tenantID][id]
	if r == nil {
		return store.ErrNotFound
	}
	r.State = upd.State
	if upd.Hash != "" {
		r.Hash = upd.Hash
	}
	if upd.Method != "" {
		r.Method = upd.Method
	}
	if upd.Detail != "" {
		r.Detail = upd.Detail
	}
	return nil
}

func (m *memStore) CountLXMFMessages(_ context.Context, tenantID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows[tenantID]), nil
}

func (m *memStore) PruneLXMFMessages(context.Context, string, int) (int, error) { return 0, nil }

func (m *memStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claims[key] {
		return false, nil
	}
	m.claims[key] = true
	return true, nil
}

func (m *memStore) ListTenants(context.Context) ([]store.Tenant, error) { return m.tenants, nil }

func (m *memStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	for i := range m.tenants {
		if m.tenants[i].ID == id {
			return &m.tenants[i], nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) states(tenantID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for id, r := range m.rows[tenantID] {
		out[id] = r.State
	}
	return out
}

func (m *memStore) inbox(tenantID string) []store.LXMFMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.LXMFMessage
	for _, r := range m.rows[tenantID] {
		if r.Direction == store.LXMFDirectionIn {
			out = append(out, *r)
		}
	}
	return out
}

type memAccounts struct {
	mu   sync.Mutex
	rows map[string]map[string]string
	// onSet mimics the bus: every replica reloads the tenant.
	onSet func(tenantID string)
}

func (a *memAccounts) ForTenant(_ context.Context, tenantID, provider string) (*integrations.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.rows[tenantID+"/"+provider]
	if f == nil {
		return nil, nil
	}
	cp := map[string]string{}
	for k, v := range f {
		cp[k] = v
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: cp}, nil
}

func (a *memAccounts) Set(_ context.Context, tenantID, provider string, fields map[string]string) (*integrations.Account, error) {
	a.mu.Lock()
	k := tenantID + "/" + provider
	if a.rows[k] == nil {
		a.rows[k] = map[string]string{}
	}
	for f, v := range fields {
		a.rows[k][f] = v
	}
	a.mu.Unlock()
	if a.onSet != nil {
		a.onSet(tenantID)
	}
	return a.ForTenant(context.Background(), tenantID, provider)
}

// peer is a stock-shaped LXMF client on one TCP connection to the hub.
type peer struct {
	node   *rns.Node
	router *lxmf.Router
	got    chan *lxmf.Message
	conn   net.Conn
	mu     sync.Mutex
}

func (p *peer) Transmit(_ string, raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.conn.Write(rnswire.HDLCFrame(raw))
	return err
}
func (p *peer) Floodable() []string { return []string{"tcp"} }
func (p *peer) HWMTU(string) int    { return rnsnode.TCPHWMTU }
func (p *peer) Bitrate(string) int  { return 0 }

func dialPeer(t *testing.T, ctx context.Context, addr, name string) *peer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	id, _ := rnswire.GenerateIdentity()
	p := &peer{conn: conn, got: make(chan *lxmf.Message, 16)}
	n, err := rns.New(rns.Config{Identity: id}, p)
	if err != nil {
		t.Fatal(err)
	}
	p.node = n
	p.router = lxmf.New(n, lxmf.Config{Identity: id, DisplayName: name, OnMessage: func(m *lxmf.Message) { p.got <- m }})
	n.Start(ctx)
	go func() {
		r := rnswire.NewHDLCFrameReader()
		buf := make([]byte, 65536)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			for _, f := range r.Feed(buf[:k]) {
				n.Inbound(f, "tcp")
			}
		}
	}()
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func h16(s string) [16]byte {
	b, _ := hex.DecodeString(s)
	var out [16]byte
	copy(out[:], b)
	return out
}

type fixture struct {
	st   *memStore
	acc  *memAccounts
	svc  *Service
	node *rnsnode.Service
	addr string
}

func newFixture(t *testing.T, ctx context.Context, cfg Config) *fixture {
	t.Helper()
	st := newMemStore(store.DefaultTenantID, "other")
	acc := &memAccounts{rows: map[string]map[string]string{}}
	svc := New(cfg, st, acc, nil)
	acc.onSet = func(tenantID string) { svc.Reload(tenantID) }
	hubID, _ := rnswire.GenerateIdentity()
	node := rnsnode.New(rnsnode.Config{Identity: hubID, BridgeListenAddr: "127.0.0.1:0", OnNode: svc.OnNode, OnNodeStopped: svc.OnNodeStopped})
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)
	return &fixture{st: st, acc: acc, svc: svc, node: node, addr: node.Status().Bridge.Addr}
}

// A peer's message to tenant A lands in A's inbox and nowhere else (the
// default tenant is the victim); A's reply goes out through the queue and is
// delivered with the peer's proof; a disabled address leaves the node.
func TestService_InboxIsPerTenantAndOutboundIsProven(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, ctx, Config{AnnounceInterval: time.Hour})
	victim, other := store.DefaultTenantID, "other"
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	oid, err := f.svc.EnsureIdentity(ctx, other, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if vid.Address == oid.Address || len(vid.Address) != 32 || vid.DisplayName != "Tenant default" {
		t.Fatalf("identities %+v %+v", vid, oid)
	}
	if again, _ := f.svc.EnsureIdentity(ctx, victim, "x"); again.Address != vid.Address {
		t.Fatal("EnsureIdentity is not idempotent")
	}
	if ok, _ := f.svc.RegisteredOnThisReplica(victim); !ok {
		t.Fatal("victim's address not registered on the node after the reload")
	}

	p := dialPeer(t, ctx, f.addr, "Sideband user")
	waitFor(t, "peer connected", func() bool { return f.node.BridgeCount() == 1 })
	// The peer announces; the hub announces both addresses (Reload did), so
	// the peer learns them once it is connected.
	if err := p.router.Announce(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hub learns the peer", func() bool { return f.node.Node().Paths().Has(p.router.Hash()) })
	f.svc.Reload(victim)
	f.svc.Reload(other)
	waitFor(t, "peer learns the victim", func() bool { return p.node.Paths().Has(h16(vid.Address)) && p.node.Paths().Has(h16(oid.Address)) })

	// Peer -> victim.
	sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
	defer scancel()
	m, _, err := p.router.Send(sctx, h16(vid.Address), []byte("hello from the mesh"), []byte("hi"), lxmf.MapOf(), 0)
	if err != nil {
		t.Fatalf("peer send: %v", err)
	}
	waitFor(t, "victim inbox", func() bool { return len(f.st.inbox(victim)) == 1 })
	in := f.st.inbox(victim)[0]
	if in.Content != "hello from the mesh" || in.Title != "hi" || in.Hash != hex.EncodeToString(m.Hash[:]) || in.Source != p.router.HashHex() ||
		in.Destination != vid.Address || !in.SignatureValid || in.Method != "opportunistic" || in.State != store.LXMFStateReceived {
		t.Fatalf("inbox row %+v", in)
	}
	if len(f.st.inbox(other)) != 0 {
		t.Fatal("the other tenant saw the victim's message")
	}
	// The same message again is filed once (the router dedups by hash and
	// the claim guards a resend across a failover).
	f.svc.onMessage(victim, m)
	if len(f.st.inbox(victim)) != 1 {
		t.Fatal("duplicate filed twice")
	}

	// Victim -> peer through the queue.
	row, err := f.svc.Send(ctx, victim, SendRequest{Destination: p.router.HashHex(), Content: "reply from the hub", Title: "re", Actor: "op@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.LXMFStateQueued || row.Source != vid.Address {
		t.Fatalf("queued row %+v", row)
	}
	select {
	case got := <-p.got:
		if string(got.Content) != "reply from the hub" || string(got.Title) != "re" || got.Source != h16(vid.Address) {
			t.Fatalf("peer got %+v", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("peer did not receive the hub's message")
	}
	waitFor(t, "delivered state", func() bool { return f.st.states(victim)[row.ID] == store.LXMFStateDelivered })
	if st := f.svc.Stats(); st.Addresses != 2 || st.Received != 1 || st.Delivered != 1 || st.Failed != 0 {
		t.Fatalf("stats %+v", st)
	}

	// Bad requests never reach the queue.
	for _, bad := range []SendRequest{
		{Destination: "nope", Content: "x"},
		{Destination: vid.Address, Content: "to myself"},
		{Destination: p.router.HashHex(), Content: ""},
		{Destination: p.router.HashHex(), Content: "x", Method: "carrier pigeon"},
	} {
		if _, err := f.svc.Send(ctx, victim, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := f.svc.Send(ctx, "third", SendRequest{Destination: p.router.HashHex(), Content: "x"}); err != ErrNoIdentity {
		t.Fatalf("tenant without identity: %v", err)
	}

	// Disable: the address leaves the node and sends are refused.
	off := false
	if _, err := f.svc.UpdateIdentity(ctx, victim, "owner@example.org", IdentityUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.svc.RegisteredOnThisReplica(victim); ok {
		t.Fatal("disabled address still registered")
	}
	if f.node.Node().LocalDestination(h16(vid.Address)) != nil {
		t.Fatal("disabled address still a local destination")
	}
	if _, err := f.svc.Send(ctx, victim, SendRequest{Destination: p.router.HashHex(), Content: "x"}); err == nil {
		t.Fatal("send accepted on a disabled address")
	}
	name, cost := "Field kits", 6
	id2, err := f.svc.UpdateIdentity(ctx, victim, "owner@example.org", IdentityUpdate{DisplayName: &name, StampCost: &cost})
	if err != nil || id2.DisplayName != "Field kits" || id2.StampCost != 6 || id2.Enabled {
		t.Fatalf("update: %+v %v", id2, err)
	}
	tooHigh := 99
	if _, err := f.svc.UpdateIdentity(ctx, victim, "x", IdentityUpdate{StampCost: &tooHigh}); err == nil {
		t.Fatal("stamp cost 99 accepted")
	}
}

// The identity export is Reticulum's own layout and loads back to the same
// address.
func TestIdentityKeyRoundTrip(t *testing.T) {
	id, _ := rnswire.GenerateIdentity()
	h := PrivateKeyHex(id)
	if len(h) != 128 {
		t.Fatalf("key hex length %d", len(h))
	}
	back, err := identityFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	if back.IdentityHash() != id.IdentityHash() || back.DestHash(lxmf.DeliveryName) != id.DestHash(lxmf.DeliveryName) {
		t.Fatal("key did not round trip")
	}
	if _, err := identityFromHex("abcd"); err == nil {
		t.Fatal("short key accepted")
	}
}

// Beyond the per-minute budget inbound messages are dropped and counted.
func TestService_InboundRateLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, ctx, Config{AnnounceInterval: time.Hour, InboundPerMinute: 2})
	vid, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	src, _ := rnswire.GenerateIdentity()
	for i := 0; i < 3; i++ {
		// Distinct contents: identical messages share a hash and the claim
		// files them once, which is dedup, not rate limiting.
		m := &lxmf.Message{Dest: h16(vid.Address), Source: src.DestHash(lxmf.DeliveryName), Content: []byte(fmt.Sprint("message ", i)), Timestamp: float64(time.Now().Unix())}
		if err := m.Pack(src, 0, nil); err != nil {
			t.Fatal(err)
		}
		f.svc.onMessage(store.DefaultTenantID, m)
	}
	if n := len(f.st.inbox(store.DefaultTenantID)); n != 2 || f.svc.Stats().Dropped != 1 {
		t.Fatalf("inbox %d dropped %d", n, f.svc.Stats().Dropped)
	}
}
