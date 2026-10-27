package lxmfhub

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// heard reports whether a peer knows an address from an announce: nothing
// here asks for a path.
func heard(p *peer, addr string) bool { return p.node.Recall(h16(addr)) != nil }

// A client that joins a node hears the addresses on it without asking for a
// path (MESHSAT-1561): MeshChat listed the workspace address as unknown for
// up to 30 minutes after connecting. A Private address is announced to its
// own members only; a Public one to whoever joins the main node.
func TestJoin_AMemberHearsTheAddressWithoutAsking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	victim := store.DefaultTenantID
	vid, err := f.svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "victim registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(victim); return ok })
	// The address's own first announce is spent before anyone joins, and
	// the next is an hour away: whatever a joiner hears, its joining caused.
	f.svc.announceOne(victim)

	stranger := dialPeerIFAC(t, ctx, f.addr, "stranger", "", "")
	member := dialPeerIFAC(t, ctx, f.addr, "member", vid.IFACNetname, vid.IFACPassphrase)
	// A client's first frame classifies its connection; its own announce is
	// what a stock client sends.
	if err := stranger.router.Announce(); err != nil {
		t.Fatal(err)
	}
	if err := member.router.Announce(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the member hears its workspace's address and propagation node", func() bool {
		return heard(member, vid.Address) && heard(member, vid.PropagationAddress)
	})
	time.Sleep(500 * time.Millisecond)
	if heard(stranger, vid.Address) || heard(stranger, vid.PropagationAddress) {
		t.Fatal("a stranger heard a Private address announced")
	}

	// Public: whoever joins the main node hears it.
	f.makePublic(t, ctx, victim)
	f.svc.announceOne(victim)
	late := dialPeerIFAC(t, ctx, f.addr, "late stranger", "", "")
	if err := late.router.Announce(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a client joining the main node hears the Public address", func() bool { return heard(late, vid.Address) })
}

// However many connections join, an address is announced again at most once
// a minute because of them, and a join inside the minute is announced when it
// is up: every joiner hears the address within a minute.
func TestJoin_AnnouncesAtMostOnceAMinute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	f := newAccessFixtureWith(t, ctx, Config{AnnounceInterval: time.Hour, Now: clock})
	for _, tid := range []string{store.DefaultTenantID, "other"} {
		if _, err := f.svc.EnsureIdentity(ctx, tid, "owner@example.org"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, tid+" registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(tid); return ok })
	}
	state := func() (time.Time, bool) {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		tr := f.svc.routers[store.DefaultTenantID]
		return tr.lastAnnounce, tr.joinPending
	}

	f.svc.OnJoin(store.DefaultTenantID)
	first, _ := state()
	if !first.Equal(now) {
		t.Fatalf("a join did not announce: %v", first)
	}
	// Another workspace's member, or a stranger on the main node, is not
	// this Private address's business.
	f.svc.OnJoin("other")
	f.svc.OnJoin("")
	if l, p := state(); !l.Equal(first) || p {
		t.Fatalf("another node's join reached this address: %v pending=%v", l, p)
	}
	advance(59 * time.Second)
	f.svc.OnJoin(store.DefaultTenantID)
	if l, p := state(); !l.Equal(first) || !p {
		t.Fatalf("a join inside the minute: announced=%v pending=%v, want held", !l.Equal(first), p)
	}
	f.svc.flushJoins()
	if l, _ := state(); !l.Equal(first) {
		t.Fatal("the held join was announced before the minute was up")
	}
	advance(2 * time.Second)
	f.svc.flushJoins()
	if l, p := state(); !l.After(first) || p {
		t.Fatalf("the held join was not announced when the minute was up: %v pending=%v", l, p)
	}
}

// The announcer itself announces a held join once the minute is up.
func TestJoin_TheAnnouncerAnnouncesAHeldJoin(t *testing.T) {
	tick := announcerTick
	announcerTick = 20 * time.Millisecond
	t.Cleanup(func() { announcerTick = tick })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newAccessFixtureWith(t, ctx, Config{AnnounceInterval: time.Hour, Now: clock})
	if _, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "registered", func() bool { ok, _ := f.svc.RegisteredOnThisReplica(store.DefaultTenantID); return ok })
	last := func() time.Time {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return f.svc.routers[store.DefaultTenantID].lastAnnounce
	}
	f.svc.OnJoin(store.DefaultTenantID)
	first := last()
	mu.Lock()
	now = now.Add(30 * time.Second)
	mu.Unlock()
	f.svc.OnJoin(store.DefaultTenantID)
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	waitFor(t, "the announcer announces the held join", func() bool { return last().After(first) })
}
