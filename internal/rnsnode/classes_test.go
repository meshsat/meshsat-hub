package rnsnode

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// dialIFACPeer is dialPeer with an interface access code on the peer's side,
// as a stock client configured with a network name and passphrase.
func dialIFACPeer(t *testing.T, ctx context.Context, addr string, f *rnswire.IFAC) *peerNode {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	id, _ := rnswire.GenerateIdentity()
	p := &peerNode{conn: conn}
	n, err := rns.New(rns.Config{Identity: id}, p)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		n.SetIFAC("tcp", f)
	}
	p.node = n
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

func mustIFAC(t *testing.T, netname, pass string) *rnswire.IFAC {
	t.Helper()
	f, err := rnswire.NewIFAC(netname, pass, rnswire.DefaultIFACSize)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newClassService(t *testing.T, ctx context.Context, cfg Config) *Service {
	t.Helper()
	id, _ := rnswire.GenerateIdentity()
	cfg.Identity = id
	cfg.BridgeListenAddr, cfg.PublicListenAddr = "127.0.0.1:0", "127.0.0.1:0"
	svc := New(cfg)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	return svc
}

func hasPathSoon(n *rns.Node, d [rns.HashLen]byte, within time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return n.RequestPath(ctx, d, within)
}

// A Private tenant's address is reachable through the one public port only
// by a connection that presented that tenant's access code: a stranger with
// none, and another tenant's member with their own, get no path and never
// hear its announce. The member in turn reaches nothing outside its
// tenant's network, and the stranger still reaches a Public address.
func TestPrivate_OnlyTheTenantsCodeReachesItsAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := newClassService(t, ctx, Config{})
	nodeA, err := svc.PrivateNode("t_a", "net-a", "pass-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrivateNode("t_b", "net-b", "pass-b"); err != nil {
		t.Fatal(err)
	}
	destA := nodeA.AddDestination(&rns.Destination{Name: "test.tenanta"})
	destM := svc.Node().AddDestination(&rns.Destination{Name: "test.public"})
	pub := svc.Status().Public.Addr

	member := dialIFACPeer(t, ctx, pub, mustIFAC(t, "net-a", "pass-a"))
	stranger := dialIFACPeer(t, ctx, pub, nil)
	other := dialIFACPeer(t, ctx, pub, mustIFAC(t, "net-b", "pass-b"))

	if !hasPathSoon(member.node, destA.Hash, 3*time.Second) {
		t.Fatal("the tenant's own member got no path to its address")
	}
	if !hasPathSoon(stranger.node, destM.Hash, 3*time.Second) {
		t.Fatal("a client without a code got no path to a Public address")
	}
	if hasPathSoon(stranger.node, destA.Hash, time.Second) {
		t.Fatal("a stranger got a path to a Private tenant's address")
	}
	if hasPathSoon(other.node, destA.Hash, time.Second) {
		t.Fatal("another tenant's member got a path to a Private tenant's address")
	}
	if hasPathSoon(member.node, destM.Hash, time.Second) {
		t.Fatal("a Private tenant's member reached the public network through the Hub")
	}

	if err := nodeA.Announce(destA, "", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if stranger.node.Paths().Has(destA.Hash) || other.node.Paths().Has(destA.Hash) {
		t.Fatal("a Private tenant's announce reached a connection outside its network")
	}

	st := svc.Status()
	if st.PrivateNodes != 2 || st.PublicPrivate != 2 || st.PublicMain != 1 || st.PublicUnclassified != 0 {
		t.Fatalf("classification %+v", st)
	}
	for _, id := range (&transmitter{s: svc}).Floodable() {
		if tn, _, _ := svc.bindingOf(id); tn != nil {
			t.Fatalf("the main node floods onto %s, a Private tenant's connection", id)
		}
	}
	if err := (&transmitter{s: svc}).Transmit("tcpp_1", []byte{0x02, 0}); err == nil {
		t.Fatal("the main node may write to a Private tenant's connection")
	}
}

// A code that matches no network is dropped and counted, and the connection
// is closed after a few such frames rather than kept open for guessing.
func TestPrivate_AWrongCodeIsDroppedAndClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := newClassService(t, ctx, Config{})
	if _, err := svc.PrivateNode("t_a", "net-a", "pass-a"); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", svc.Status().Public.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	wrong := mustIFAC(t, "net-a", "not-the-passphrase")
	for i := 0; i < maxUnmatched; i++ {
		f := make([]byte, rnswire.HeaderMinSize+8)
		f[0], f[2] = 0x02, byte(i)
		if _, err := conn.Write(rnswire.HDLCFrame(wrong.Wrap(f))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "connection closed", func() bool { return len(svc.Status().Public.Connections) == 0 })
	if st := svc.Status(); st.IFACUnmatched != maxUnmatched || st.PublicPrivate != 0 {
		t.Fatalf("status %+v", st)
	}
}

// A new passphrase locks out every connection that joined with the old one
// at once; releasing the tenant's node does the same and stops it.
func TestPrivate_RotationAndReleaseCloseTheTenantsConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := newClassService(t, ctx, Config{})
	nodeA, err := svc.PrivateNode("t_a", "net-a", "pass-a")
	if err != nil {
		t.Fatal(err)
	}
	destA := nodeA.AddDestination(&rns.Destination{Name: "test.tenanta"})
	pub := svc.Status().Public.Addr
	member := dialIFACPeer(t, ctx, pub, mustIFAC(t, "net-a", "pass-a"))
	if !hasPathSoon(member.node, destA.Hash, 3*time.Second) {
		t.Fatal("member not admitted")
	}
	same, err := svc.PrivateNode("t_a", "net-a", "pass-a")
	if err != nil || same != nodeA || svc.Status().PublicPrivate != 1 {
		t.Fatal("re-registering with the same code must keep the node and its connections")
	}

	if _, err := svc.PrivateNode("t_a", "net-a", "pass-new"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old-code connection closed", func() bool { return svc.Status().PublicPrivate == 0 })
	stale := dialIFACPeer(t, ctx, pub, mustIFAC(t, "net-a", "pass-a"))
	if hasPathSoon(stale.node, destA.Hash, time.Second) {
		t.Fatal("the old passphrase still works after rotation")
	}
	fresh := dialIFACPeer(t, ctx, pub, mustIFAC(t, "net-a", "pass-new"))
	if !hasPathSoon(fresh.node, destA.Hash, 3*time.Second) {
		t.Fatal("the new passphrase does not work")
	}

	svc.ReleasePrivate("t_a")
	waitFor(t, "released tenant's connections closed", func() bool { return svc.Status().PublicPrivate == 0 })
	if svc.Status().PrivateNodes != 0 {
		t.Fatal("released node still listed")
	}
	if _, err := svc.PrivateNode("", "", ""); err == nil {
		t.Fatal("a private node without a code must be refused")
	}
}

// A connection that never says anything is in no node's interface list and
// is closed after the timeout; the edge's health checks look exactly so.
func TestPrivate_UnclassifiedConnectionsAreClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := newClassService(t, ctx, Config{UnclassifiedTimeout: 400 * time.Millisecond})
	conn, err := net.Dial("tcp", svc.Status().Public.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	waitFor(t, "unclassified connection listed", func() bool { return svc.Status().PublicUnclassified == 1 })
	for _, id := range (&transmitter{s: svc}).Floodable() {
		if isPublic(id) {
			t.Fatalf("an unclassified connection %s is floodable", id)
		}
	}
	waitFor(t, "unclassified connection closed", func() bool { return len(svc.Status().Public.Connections) == 0 })
}

// With the platform's own access code set, the main node admits only that
// code: a client without one is not bound anywhere.
func TestPrivate_PlatformCodeGuardsTheMainNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := mustIFAC(t, "net-platform", "pass-platform")
	svc := newClassService(t, ctx, Config{PublicIFAC: platform})
	destM := svc.Node().AddDestination(&rns.Destination{Name: "test.public"})
	pub := svc.Status().Public.Addr
	bare := dialIFACPeer(t, ctx, pub, nil)
	if hasPathSoon(bare.node, destM.Hash, time.Second) {
		t.Fatal("a client without the platform's code reached the main node")
	}
	keyed := dialIFACPeer(t, ctx, pub, platform)
	if !hasPathSoon(keyed.node, destM.Hash, 3*time.Second) {
		t.Fatal("the platform's code does not admit to the main node")
	}
	if st := svc.Status(); st.PublicMain != 1 || st.IFACUnmatched == 0 {
		t.Fatalf("the client without the code must not be bound to the main node: %+v", st)
	}
}

func TestTryBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTryBudget(2)
	for i := 0; i < 2; i++ {
		if !b.allow(now, 1) {
			t.Fatalf("try %d refused within a budget of 2", i+1)
		}
	}
	if b.allow(now, 1) {
		t.Fatal("budget of 2 per second must refuse the third try at once")
	}
	if !b.allow(now.Add(time.Second), 2) {
		t.Fatal("budget must refill")
	}
	if b.allow(now.Add(time.Second), 3) {
		t.Fatal("a batch larger than the budget must be refused")
	}
}
