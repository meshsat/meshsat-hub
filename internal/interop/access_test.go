package interop

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/rnsnode"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The access modes (MESHSAT-1722) against stock Python RNS/LXMF clients on
// the Hub's PUBLIC listener, as production assembles it: a client that
// enters the tenant's network name and passphrase reaches a Private address
// and delivers to the inbox; one without a code, or with another tenant's,
// gets no path; Public admits anyone; Off admits nobody.
func TestInteropAccessModesOnThePublicListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	victim, other := store.DefaultTenantID, "other"
	st := lxmfhubtest.NewMemStore(victim, other)
	acc := lxmfhubtest.NewMemAccounts()
	svc := lxmfhub.New(lxmfhub.Config{AnnounceInterval: time.Hour}, st, acc, nil)
	acc.OnSet = func(tenantID string) { svc.Reload(tenantID) }
	hid, _ := rnswire.GenerateIdentity()
	node := rnsnode.New(rnsnode.Config{Identity: hid, BridgeListenAddr: "127.0.0.1:0", PublicListenAddr: "127.0.0.1:0",
		OnNode: svc.OnNode, OnNodeStopped: svc.OnNodeStopped})
	svc.SetNodes(node)
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)
	vid, err := svc.EnsureIdentity(ctx, victim, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	oid, err := svc.EnsureIdentity(ctx, other, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if vid.AccessMode != lxmfhub.AccessPrivate {
		t.Fatalf("a new address is %q, want private", vid.AccessMode)
	}
	_, portStr, _ := net.SplitHostPort(node.Status().Public.Addr)
	port, _ := strconv.Atoi(portStr)
	pub := fmt.Sprintf("127.0.0.1:%d", port)

	hasPath := func(p *peer, dest string, within time.Duration) bool {
		deadline := time.Now().Add(within)
		p.send(event{"cmd": "request_path", "dest": dest})
		for time.Now().Before(deadline) {
			p.send(event{"cmd": "has_path", "dest": dest})
			if e := p.wait("path", func(e event) bool { return e["dest"] == dest }, 5*time.Second); e["known"] == true {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}

	member := startPeer(t, peerOpts{name: "member", connect: pub, ifacNetname: vid.IFACNetname, ifacPass: vid.IFACPassphrase})
	stranger := startPeer(t, peerOpts{name: "stranger", connect: pub})
	otherMember := startPeer(t, peerOpts{name: "other member", connect: pub, ifacNetname: oid.IFACNetname, ifacPass: oid.IFACPassphrase})
	waitFor(t, "three public clients", 15*time.Second, func() bool { return len(node.Status().Public.Connections) == 3 })

	if !hasPath(member, vid.Address, 20*time.Second) {
		t.Fatal("Private: the tenant's own member (stock RNS with the network name and passphrase) got no path")
	}
	member.send(event{"cmd": "send_lxm", "dest": vid.Address, "content": "from a member", "title": "private", "method": "direct"})
	member.wait("lxm_sent", nil, 10*time.Second)
	member.wait("lxm_delivered", nil, 90*time.Second)
	waitFor(t, "victim inbox", 10*time.Second, func() bool { return len(st.Inbox(victim)) == 1 })
	if hasPath(stranger, vid.Address, 8*time.Second) {
		t.Fatal("Private: a stock client without a code got a path")
	}
	if hasPath(otherMember, vid.Address, 8*time.Second) {
		t.Fatal("Private: another tenant's member got a path")
	}
	if s := node.Status(); s.PublicPrivate != 2 || s.PublicMain != 1 || s.PrivateNodes != 2 {
		t.Fatalf("classification %+v", s)
	}

	public := lxmfhub.AccessPublic
	if _, err := svc.UpdateIdentity(ctx, victim, "owner@example.org", lxmfhub.IdentityUpdate{AccessMode: &public}); err != nil {
		t.Fatal(err)
	}
	if !hasPath(stranger, vid.Address, 20*time.Second) {
		t.Fatal("Public: a stock client without a code got no path")
	}
	stranger.send(event{"cmd": "send_lxm", "dest": vid.Address, "content": "from anyone", "title": "public", "method": "direct"})
	stranger.wait("lxm_sent", nil, 10*time.Second)
	stranger.wait("lxm_delivered", nil, 90*time.Second)
	waitFor(t, "victim inbox, second message", 10*time.Second, func() bool { return len(st.Inbox(victim)) == 2 })

	off := lxmfhub.AccessOff
	if _, err := svc.UpdateIdentity(ctx, victim, "owner@example.org", lxmfhub.IdentityUpdate{AccessMode: &off}); err != nil {
		t.Fatal(err)
	}
	late := startPeer(t, peerOpts{name: "late", connect: pub})
	if hasPath(late, vid.Address, 8*time.Second) {
		t.Fatal("Off: a stock client got a path")
	}
}
