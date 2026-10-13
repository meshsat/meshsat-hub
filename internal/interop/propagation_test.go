package interop

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/rnsnode"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// hubLXMF is the Hub's LXMF service on its node: a tenant's address and its
// propagation node, exactly as production assembles them, on in-memory
// store and account fakes.
type hubLXMF struct {
	node *rnsnode.Service
	svc  *lxmfhub.Service
	st   *lxmfhubtest.MemStore
	port int
}

func newHubLXMF(t *testing.T, ctx context.Context, pnCost int) (*hubLXMF, *lxmfhub.Identity) {
	t.Helper()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID)
	acc := lxmfhubtest.NewMemAccounts()
	svc := lxmfhub.New(lxmfhub.Config{AnnounceInterval: time.Hour}, st, acc, nil)
	acc.OnSet = func(tenantID string) { svc.Reload(tenantID) }
	hid, _ := rnswire.GenerateIdentity()
	node := rnsnode.New(rnsnode.Config{Identity: hid, BridgeListenAddr: "127.0.0.1:0", OnNode: svc.OnNode, OnNodeStopped: svc.OnNodeStopped})
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)
	if _, err := svc.EnsureIdentity(ctx, store.DefaultTenantID, "test"); err != nil {
		t.Fatal(err)
	}
	cost := pnCost
	if _, err := svc.UpdateIdentity(ctx, store.DefaultTenantID, "test", lxmfhub.IdentityUpdate{PropagationStampCost: &cost}); err != nil {
		t.Fatal(err)
	}
	id, _ := svc.LoadIdentity(ctx, store.DefaultTenantID)
	_, portStr, _ := net.SplitHostPort(node.Status().Bridge.Addr)
	port, _ := strconv.Atoi(portStr)
	return &hubLXMF{node: node, svc: svc, st: st, port: port}, id
}

// Python A hands the Hub tenant's propagation node a message for Python B
// (stamp at the node's cost); the node holds it; B syncs and receives it as
// a propagated message; B's next sync purges it from the node. A message
// handed in for the tenant's own address lands straight in the inbox.
func TestInteropLXMFPropagationHoldAndSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub, id := newHubLXMF(t, ctx, 8)
	pa := startPeer(t, peerOpts{name: "A", connect: fmt.Sprintf("127.0.0.1:%d", hub.port)})
	pb := startPeer(t, peerOpts{name: "B", connect: fmt.Sprintf("127.0.0.1:%d", hub.port)})
	waitFor(t, "two peers", 10*time.Second, func() bool { return hub.node.BridgeCount() >= 2 })

	// Everybody learns everybody: B announces (A hears it through the Hub),
	// the Hub announces the tenant's address and propagation node.
	defer func() {
		if t.Failed() {
			st := hub.node.Status()
			t.Logf("hub node: active %v paths %d links %d stats %+v bridge conns %+v", st.Active, st.Paths, st.Links, st.Stats, st.Bridge.Connections)
			for _, pi := range hub.node.Paths() {
				t.Logf("hub path: %+v", pi)
			}
			pa.mu.Lock()
			for _, ev := range pa.log {
				t.Logf("A saw: %v", ev)
			}
			pa.mu.Unlock()
			if data, err := os.ReadFile(filepath.Join(pa.cfgDir, "rns.log")); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if strings.Contains(line, "rror") || strings.Contains(line, "xception") || strings.Contains(line, "allback") {
						t.Logf("A rns.log: %s", line)
					}
				}
			}
		}
	}()
	pb.send(event{"cmd": "announce"})
	waitFor(t, "hub learns B's delivery", 10*time.Second, func() bool { return hub.node.Node() != nil && hub.node.Node().Paths().Hops(h16(pb.Delivery)) == 1 })
	hub.svc.Reload(store.DefaultTenantID)
	// What a sending client needs is the path and the identity, which the
	// path table proves; the announce event is a courtesy of the peer script.
	knows := func(p *peer, dest string) func() bool {
		return func() bool {
			p.send(event{"cmd": "has_path", "dest": dest})
			e := p.wait("path", func(e event) bool { return e["dest"] == dest }, 5*time.Second)
			return e["known"] == true
		}
	}
	waitFor(t, "A knows B's delivery", 20*time.Second, knows(pa, pb.Delivery))
	waitFor(t, "A knows the propagation node", 20*time.Second, knows(pa, id.PropagationAddress))
	waitFor(t, "B knows the propagation node", 20*time.Second, knows(pb, id.PropagationAddress))
	waitFor(t, "A knows the tenant address", 20*time.Second, knows(pa, id.Address))
	// B verifies A's signature only once it has heard A announce.
	pa.send(event{"cmd": "announce"})
	waitFor(t, "B knows A's delivery", 20*time.Second, knows(pb, pa.Delivery))

	// A -> B via the propagation node. The client makes the propagation stamp
	// at the node's announced cost before it sends.
	pa.send(event{"cmd": "set_pn", "dest": id.PropagationAddress})
	pa.wait("pn_set", nil, 5*time.Second)
	pa.send(event{"cmd": "send_lxm", "dest": pb.Delivery, "content": "held for you", "title": "pn", "method": "propagated"})
	sent := pa.wait("lxm_sent", nil, 10*time.Second)
	pa.wait("lxm_delivered", nil, 90*time.Second) // delivered to the node
	waitFor(t, "node holds one message", 10*time.Second, func() bool { return hub.st.HeldCount(store.DefaultTenantID) == 1 })

	// B syncs: list, fetch, receive.
	pb.send(event{"cmd": "set_pn", "dest": id.PropagationAddress})
	pb.wait("pn_set", nil, 5*time.Second)
	pb.send(event{"cmd": "sync_pn"})
	rcv := pb.wait("lxm_received", nil, 60*time.Second)
	if rcv["content"] != "held for you" || rcv["title"] != "pn" || rcv["hash"] != sent["hash"] || rcv["signature_validated"] != true {
		t.Fatalf("B received %v", rcv)
	}
	done := pb.wait("pn_sync_done", nil, 30*time.Second)
	if done["state"].(float64) != 7 { // PR_COMPLETE
		t.Fatalf("sync ended in state %v", done)
	}
	// The next sync tells the node what B has; the node forgets it.
	pb.send(event{"cmd": "sync_pn"})
	pb.wait("pn_sync_done", nil, 30*time.Second)
	waitFor(t, "node purged the message", 10*time.Second, func() bool { return hub.st.HeldCount(store.DefaultTenantID) == 0 })
	if st := hub.svc.Stats(); st.HeldIn != 1 || st.HeldServed != 1 {
		t.Fatalf("stats %+v", st)
	}

	// A -> the tenant's own address through its propagation node: decrypted
	// and filed at once, never held.
	pa.send(event{"cmd": "send_lxm", "dest": id.Address, "content": "for the hub itself", "title": "direct to inbox", "method": "propagated"})
	pa.wait("lxm_delivered", nil, 90*time.Second)
	waitFor(t, "inbox row", 10*time.Second, func() bool { return len(hub.st.Inbox(store.DefaultTenantID)) == 1 })
	in := hub.st.Inbox(store.DefaultTenantID)[0]
	if in.Content != "for the hub itself" || in.Method != "propagated" || in.Source != pa.Delivery || !in.SignatureValid {
		t.Fatalf("inbox row %+v", in)
	}
	if hub.st.HeldCount(store.DefaultTenantID) != 0 {
		t.Fatal("a message for a local address was held")
	}
	_ = hex.EncodeToString
}
