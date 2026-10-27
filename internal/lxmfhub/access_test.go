package lxmfhub

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnsnode"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The access modes (MESHSAT-1722) end to end in one process: the Hub's
// node with its public listener, the Private tenants' own nodes, and
// stock-shaped LXMF clients with and without a tenant's code.

func newAccessFixture(t *testing.T, ctx context.Context) *fixture {
	t.Helper()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID, "other")
	acc := lxmfhubtest.NewMemAccounts()
	svc := New(Config{AnnounceInterval: time.Hour}, st, acc, nil)
	acc.OnSet = func(tenantID string) { svc.Reload(tenantID) }
	hubID, _ := rnswire.GenerateIdentity()
	node := rnsnode.New(rnsnode.Config{Identity: hubID, BridgeListenAddr: "127.0.0.1:0", PublicListenAddr: "127.0.0.1:0",
		OnNode: svc.OnNode, OnNodeStopped: svc.OnNodeStopped})
	svc.SetNodes(node)
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)
	return &fixture{st: st, acc: acc, svc: svc, node: node, addr: node.Status().Public.Addr}
}

func dialPeerIFAC(t *testing.T, ctx context.Context, addr, name, netname, pass string) *peer {
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
	if netname != "" || pass != "" {
		f, err := rnswire.NewIFAC(netname, pass, rnswire.DefaultIFACSize)
		if err != nil {
			t.Fatal(err)
		}
		n.SetIFAC("tcp", f)
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
			for _, fr := range r.Feed(buf[:k]) {
				n.Inbound(fr, "tcp")
			}
		}
	}()
	return p
}

func pathSoon(p *peer, addr string, within time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return p.node.RequestPath(ctx, h16(addr), within)
}

func (f *fixture) setMode(t *testing.T, ctx context.Context, tenantID, mode string) *Identity {
	t.Helper()
	id, err := f.svc.UpdateIdentity(ctx, tenantID, "owner@example.org", IdentityUpdate{AccessMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A new address is Private: its own member reaches it and delivers a
// message to the inbox; a stranger and another tenant's member get no path.
// The default tenant is the victim.
func TestAccess_PrivateIsTheDefaultAndOnlyItsCodeReachesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	victim := store.DefaultTenantID
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	oid, err := f.svc.EnsureIdentity(ctx, "other", "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if vid.AccessMode != AccessPrivate || oid.AccessMode != AccessPrivate || vid.IFACPassphrase == oid.IFACPassphrase || len(vid.IFACPassphrase) < 50 {
		t.Fatalf("new addresses must be Private with codes of their own: %+v %+v", vid, oid)
	}
	waitFor(t, "victim registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(victim); return ok })
	if st := f.node.Status(); st.PrivateNodes != 2 {
		t.Fatalf("private nodes %d", st.PrivateNodes)
	}

	member := dialPeerIFAC(t, ctx, f.addr, "member", vid.IFACNetname, vid.IFACPassphrase)
	stranger := dialPeerIFAC(t, ctx, f.addr, "stranger", "", "")
	otherMember := dialPeerIFAC(t, ctx, f.addr, "other member", oid.IFACNetname, oid.IFACPassphrase)

	if !pathSoon(member, vid.Address, 5*time.Second) {
		t.Fatal("the tenant's own member got no path to its address")
	}
	sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
	defer scancel()
	if _, _, err := member.router.Send(sctx, h16(vid.Address), []byte("from a member"), nil, lxmf.MapOf(), 0); err != nil {
		t.Fatalf("member send: %v", err)
	}
	waitFor(t, "victim inbox", func() bool { return len(f.st.Inbox(victim)) == 1 })
	if pathSoon(stranger, vid.Address, time.Second) {
		t.Fatal("a stranger got a path to a Private address")
	}
	if pathSoon(otherMember, vid.Address, time.Second) {
		t.Fatal("another tenant's member got a path to a Private address")
	}
	if len(f.st.Inbox("other")) != 0 {
		t.Fatal("the other tenant saw the victim's message")
	}

	// A propagation node files a message for a local address only on the
	// same node: across networks it would be a way in.
	f.svc.mu.Lock()
	vNode := f.svc.routers[victim].node
	main := f.svc.node
	f.svc.mu.Unlock()
	if f.svc.routerForAddress(main, h16(vid.Address)) != nil {
		t.Fatal("the main node's propagation node would file a message into a Private address")
	}
	if f.svc.routerForAddress(vNode, h16(vid.Address)) == nil {
		t.Fatal("the tenant's own propagation node does not file for its address")
	}
}

// Public puts the address on the main node for anyone; Off takes it off
// every node and stops the tenant's own; back to Private needs the code.
func TestAccess_ModeSwitchMovesTheAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	victim := store.DefaultTenantID
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateIdentity(ctx, victim, "x", IdentityUpdate{AccessMode: ptr("bogus")}); err == nil {
		t.Fatal("an unknown access mode must be refused")
	}

	f.setMode(t, ctx, victim, AccessPublic)
	if st := f.node.Status(); st.PrivateNodes != 0 {
		t.Fatal("leaving Private must stop the tenant's own node")
	}
	anyone := dialPeerIFAC(t, ctx, f.addr, "anyone", "", "")
	if !pathSoon(anyone, vid.Address, 5*time.Second) {
		t.Fatal("a Public address is not reachable without a code")
	}
	// A transport-capable client that heard the address while it was Public
	// keeps it in its cache and answers a flooded path request from it:
	// nothing the Hub does erases other nodes' caches. What Off promises is
	// that the Hub itself no longer serves or remembers it.
	_ = anyone.conn.Close()
	waitFor(t, "the Public client gone", func() bool { return f.node.Status().PublicMain == 0 })
	// What that echo leaves on the main node: a cached path to the address.
	learned := anyone.node.Paths().Get(h16(vid.Address))
	if learned == nil {
		t.Fatal("the Public client has no path to copy")
	}
	echo := *learned
	echo.Iface = "tcpp_99"
	f.node.Node().Paths().Put(&echo)

	f.setMode(t, ctx, victim, AccessOff)
	if ok, _ := f.svc.RegisteredOnThisReplica(victim); ok {
		t.Fatal("an Off address is still registered")
	}
	if n := f.node.Node(); n.LocalDestination(h16(vid.Address)) != nil || n.Paths().Has(h16(vid.Address)) {
		t.Fatal("the main node still holds or remembers an Off address")
	}
	if pathSoon(dialPeerIFAC(t, ctx, f.addr, "anyone again", "", ""), vid.Address, time.Second) {
		t.Fatal("an Off address is reachable")
	}

	back := f.setMode(t, ctx, victim, AccessPrivate)
	if back.IFACNetname != vid.IFACNetname || back.IFACPassphrase != vid.IFACPassphrase {
		t.Fatal("returning to Private must keep the tenant's code")
	}
	if pathSoon(dialPeerIFAC(t, ctx, f.addr, "stranger", "", ""), vid.Address, time.Second) {
		t.Fatal("back in Private, a stranger still reaches the address")
	}
	if !pathSoon(dialPeerIFAC(t, ctx, f.addr, "member", vid.IFACNetname, vid.IFACPassphrase), vid.Address, 5*time.Second) {
		t.Fatal("back in Private, the member does not reach the address")
	}
}

// An address made before the modes existed has no access_mode and no code:
// it is Private, and the leader gives it a code once.
func TestAccess_AnOldRowBecomesPrivateWithACode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	victim := store.DefaultTenantID
	fresh, _ := rnswire.GenerateIdentity()
	if _, err := f.acc.Set(ctx, victim, integrations.ProviderLXMF, map[string]string{
		"private_key": PrivateKeyHex(fresh), "display_name": "before 1722", "stamp_cost": "0", "enabled": "true",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old row registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(victim); return ok })
	id, err := f.svc.LoadIdentity(ctx, victim)
	if err != nil || id.AccessMode != AccessPrivate || id.IFACNetname == "" || id.IFACPassphrase == "" {
		t.Fatalf("old row after registration: %+v %v", id, err)
	}
	f.svc.Reload(victim)
	again, _ := f.svc.LoadIdentity(ctx, victim)
	if again.IFACPassphrase != id.IFACPassphrase {
		t.Fatal("the code changed on a second registration")
	}
	if f.node.Status().PrivateNodes != 1 {
		t.Fatal("old row is not on a private node")
	}
}

// Rotation locks out the connections that joined with the old code at once,
// and only the new code admits.
func TestAccess_RotateLocksOutTheOldCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	victim := store.DefaultTenantID
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(victim); return ok })
	member := dialPeerIFAC(t, ctx, f.addr, "member", vid.IFACNetname, vid.IFACPassphrase)
	if !pathSoon(member, vid.Address, 5*time.Second) {
		t.Fatal("member not admitted")
	}
	rot, err := f.svc.RotateAccess(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if rot.IFACPassphrase == vid.IFACPassphrase || rot.IFACNetname == vid.IFACNetname {
		t.Fatal("rotation kept the old code")
	}
	waitFor(t, "old member disconnected", func() bool { return f.node.Status().PublicPrivate == 0 })
	if pathSoon(dialPeerIFAC(t, ctx, f.addr, "old code", vid.IFACNetname, vid.IFACPassphrase), vid.Address, time.Second) {
		t.Fatal("the old code still admits after rotation")
	}
	if !pathSoon(dialPeerIFAC(t, ctx, f.addr, "new code", rot.IFACNetname, rot.IFACPassphrase), vid.Address, 5*time.Second) {
		t.Fatal("the new code does not admit")
	}
}

func ptr[T any](v T) *T { return &v }

// A leader takes over with addresses already stored: Private, Off, and one
// from before the modes (no code). rnsnode.Start calls OnNode holding its
// own lock, and registration calls back into rnsnode (PrivateNode,
// ForgetPaths); a call that takes that lock again hangs the leader for good,
// and no test that creates addresses after the start can see it.
func TestAccess_LeaderStartRegistersStoredAddressesWithoutDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID, "other", "old")
	acc := lxmfhubtest.NewMemAccounts()
	svc := New(Config{AnnounceInterval: time.Hour}, st, acc, nil)
	acc.OnSet = func(tenantID string) { svc.Reload(tenantID) }
	for tid, fields := range map[string]map[string]string{
		store.DefaultTenantID: {"access_mode": AccessPrivate, "ifac_netname": "meshsat-aaaaaaaa", "ifac_passphrase": "p-default"},
		"other":               {"access_mode": AccessOff},
		"old":                 {},
	} {
		id, _ := rnswire.GenerateIdentity()
		fields["private_key"], fields["display_name"], fields["enabled"] = PrivateKeyHex(id), "stored "+tid, "true"
		if _, err := acc.Set(ctx, tid, integrations.ProviderLXMF, fields); err != nil {
			t.Fatal(err)
		}
	}
	hubID, _ := rnswire.GenerateIdentity()
	node := rnsnode.New(rnsnode.Config{Identity: hubID, BridgeListenAddr: "127.0.0.1:0", PublicListenAddr: "127.0.0.1:0",
		OnNode: svc.OnNode, OnNodeStopped: svc.OnNodeStopped})
	svc.SetNodes(node)
	started := make(chan error, 1)
	go func() { started <- node.Start(ctx) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the leader's node did not start: registering stored addresses deadlocked")
	}
	t.Cleanup(node.Stop)
	if ok, _ := svc.RegisteredOnThisReplica(store.DefaultTenantID); !ok {
		t.Fatal("the stored Private address is not registered")
	}
	if ok, _ := svc.RegisteredOnThisReplica("other"); ok {
		t.Fatal("the stored Off address is registered")
	}
	waitFor(t, "the old row registered with a code", func() bool { ok, _ := svc.RegisteredOnThisReplica("old"); return ok })
	if id, _ := svc.LoadIdentity(ctx, "old"); id.IFACPassphrase == "" {
		t.Fatal("the old row has no code")
	}
	if st := node.Status(); st.PrivateNodes != 2 {
		t.Fatalf("private nodes %d, want 2", st.PrivateNodes)
	}
}
