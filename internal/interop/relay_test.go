package interop

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The Hub as a transport between two stock clients must relay BOTH kinds of
// announce a client makes: the plain one and the LXMF delivery one, which
// carries a ratchet. Sideband users reach each other through the Hub only if
// the second arrives (MESHSAT-1555 P5 found the delivery announce missing).
func TestInteropTransportRelaysRatchetAnnounces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newHub(t, ctx)
	pa := startPeer(t, peerOpts{name: "A", connect: fmt.Sprintf("127.0.0.1:%d", b.port)})
	pb := startPeer(t, peerOpts{name: "B", connect: fmt.Sprintf("127.0.0.1:%d", b.port)})
	waitFor(t, "two peers", 10*time.Second, func() bool { return b.peers() >= 2 })
	pb.send(event{"cmd": "announce"})
	waitFor(t, "hub learns B's delivery", 10*time.Second, func() bool { return b.node.Paths().Hops(h16(pb.Delivery)) == 1 })
	raw := pa.wait("announce", func(e event) bool { return e["dest"] == pb.Raw }, 15*time.Second)
	t.Logf("A heard B's raw destination at %v hops", raw["hops"])
	del := pa.wait("announce", func(e event) bool { return e["dest"] == pb.Delivery }, 15*time.Second)
	if del["hops"].(float64) != 2 {
		t.Fatalf("A heard B's delivery destination at %v hops, want 2", del["hops"])
	}
}
