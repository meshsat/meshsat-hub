package rnsnode

import (
	"context"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// memStore is the path store in memory.
type memStore struct {
	mu   sync.Mutex
	rows map[string]store.RNSPath
}

func (m *memStore) UpsertRNSPath(_ context.Context, p *store.RNSPath) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[p.DestHash] = *p
	return nil
}

func (m *memStore) ListRNSPaths(context.Context) ([]store.RNSPath, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.RNSPath, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) DeleteRNSPath(_ context.Context, dest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, dest)
	return nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// fakeMQTT captures what the node publishes.
type fakeMQTT struct {
	mu        sync.Mutex
	published [][]byte
}

func (f *fakeMQTT) Publish(raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, append([]byte(nil), raw...))
	return nil
}
func (f *fakeMQTT) Connected() bool { return true }

func (f *fakeMQTT) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.published) == 0 {
		return nil
	}
	return f.published[len(f.published)-1]
}

// peerNode is a stock-shaped rns.Node on the far end of one TCP connection.
type peerNode struct {
	node *rns.Node
	conn net.Conn
	mu   sync.Mutex
}

func (p *peerNode) Transmit(_ string, raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.conn.Write(rnswire.HDLCFrame(raw))
	return err
}
func (p *peerNode) Floodable() []string { return []string{"tcp"} }
func (p *peerNode) HWMTU(string) int    { return TCPHWMTU }
func (p *peerNode) Bitrate(string) int  { return 0 }

func dialPeer(t *testing.T, ctx context.Context, addr string) *peerNode {
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
			for _, f := range r.Feed(buf[:k]) {
				n.Inbound(f, "tcp")
			}
		}
	}()
	return p
}

func newService(t *testing.T, st Store, mq MQTTTransport, declined *[][]byte) *Service {
	t.Helper()
	id, _ := rnswire.GenerateIdentity()
	var dmu sync.Mutex
	svc := New(Config{Identity: id, Store: st, MQTT: mq, BridgeListenAddr: "127.0.0.1:0", PublicListenAddr: "127.0.0.1:0",
		OnDeclinedTCP: func(raw []byte) {
			dmu.Lock()
			defer dmu.Unlock()
			*declined = append(*declined, append([]byte(nil), raw...))
		}})
	return svc
}

// A peer's announce over TCP becomes a path on the leader's node, is written
// to the store, and comes back after a stop and start (a failover) while
// the listener is closed in between.
func TestService_LearnsPersistsAndReloadsPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &memStore{rows: map[string]store.RNSPath{}}
	var declined [][]byte
	svc := newService(t, st, nil, &declined)
	if svc.Active() || svc.InboundMQTT([]byte{1, 2, 3}) {
		t.Fatal("inactive service must not be active or consume MQTT frames")
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()
	status := svc.Status()
	if !status.Active || status.Terms != 1 || status.Bridge == nil || status.Public == nil || status.TransportID == "" {
		t.Fatalf("status after start: %+v", status)
	}

	peer := dialPeer(t, ctx, status.Bridge.Addr)
	dest := peer.node.AddDestination(&rns.Destination{Name: "test.peer"})
	waitFor(t, "peer connected", func() bool { return svc.BridgeCount() == 1 })
	if err := peer.node.Announce(dest, "", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "path learned", func() bool { return svc.Node() != nil && svc.Node().Paths().Has(dest.Hash) })
	waitFor(t, "path persisted", func() bool { return st.count() == 1 })
	paths := svc.Paths()
	if len(paths) != 1 || paths[0].Dest != hex.EncodeToString(dest.Hash[:]) || !paths[0].Direct || paths[0].Iface != "tcpb_1" {
		t.Fatalf("paths %+v", paths)
	}

	// Failover: the node stops with its listeners, and the next term starts
	// with the path already known.
	svc.Stop()
	if svc.Active() || svc.Listening() || svc.Status().Active {
		t.Fatal("stopped service still active")
	}
	if _, err := net.DialTimeout("tcp", status.Bridge.Addr, time.Second); err == nil {
		t.Fatal("listener still open after stop")
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if n := svc.Node(); n == nil || !n.Paths().Has(dest.Hash) || n.Recall(dest.Hash) == nil {
		t.Fatal("path not reloaded from the store on the second term")
	}
	if svc.Status().Terms != 2 {
		t.Fatalf("terms %d", svc.Status().Terms)
	}
}

// A frame the node does not handle reaches the legacy handler unchanged
// (hop byte included), and a frame it does handle never does.
func TestService_DeclinedFramesFallThroughUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var declined [][]byte
	var dmu sync.Mutex
	id, _ := rnswire.GenerateIdentity()
	svc := New(Config{Identity: id, BridgeListenAddr: "127.0.0.1:0", OnDeclinedTCP: func(raw []byte) {
		dmu.Lock()
		defer dmu.Unlock()
		declined = append(declined, append([]byte(nil), raw...))
	}})
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()
	conn, err := net.Dial("tcp", svc.Status().Bridge.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	waitFor(t, "connected", func() bool { return svc.BridgeCount() == 1 })

	// A HEADER_1 DATA packet to a PLAIN destination that is not the path
	// request destination: a MeshSat-private frame in the node's eyes.
	legacy := make([]byte, rnswire.HeaderMinSize+6)
	legacy[0] = 0x02 // header type 1, plain destination, data
	legacy[1] = 3    // hops as sent
	for i := 2; i < len(legacy); i++ {
		legacy[i] = 0x5a
	}
	if _, err := conn.Write(rnswire.HDLCFrame(legacy)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "declined frame", func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return len(declined) == 1
	})
	dmu.Lock()
	got := declined[0]
	dmu.Unlock()
	if got[1] != 3 || len(got) != len(legacy) {
		t.Fatalf("legacy handler saw a rewritten frame: %x", got)
	}
}

// What the node publishes on the MQTT topic comes back from the broker to
// this replica's own subscription; the echo is swallowed, a peer's frame on
// the same topic is not.
func TestService_MQTTEchoIsSwallowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mq := &fakeMQTT{}
	id, _ := rnswire.GenerateIdentity()
	var local *rns.Destination
	svc := New(Config{Identity: id, BridgeListenAddr: "127.0.0.1:0", MQTT: mq,
		OnNode: func(n *rns.Node) { local = n.AddDestination(&rns.Destination{Name: "hub.test"}) }})
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()
	if err := svc.Node().Announce(local, "", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "announce on mqtt", func() bool { return mq.last() != nil })
	before := svc.Node().Stats().RxPackets
	if !svc.InboundMQTT(mq.last()) {
		t.Fatal("echo not consumed")
	}
	if svc.Node().Stats().RxPackets != before {
		t.Fatal("echo was processed as a received packet")
	}
	// A second copy is no echo any more: it goes through the node, which
	// counts it and consumes it as its own announce (never a legacy frame).
	if !svc.InboundMQTT(mq.last()) || svc.Node().Stats().RxPackets != before+1 {
		t.Fatalf("second copy: consumed by the node as a received packet expected, rx %d -> %d", before, svc.Node().Stats().RxPackets)
	}

	// A peer's announce on the topic is a real packet: learned as a path via mqtt_rns.
	pid, _ := rnswire.GenerateIdentity()
	ann, err := rnswire.NewAnnounce(pid, "peer.mqtt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.InboundMQTT(ann.MarshalPacket()) {
		t.Fatal("peer announce not consumed")
	}
	if e := svc.Node().Paths().Get(pid.DestHash("peer.mqtt")); e == nil || e.Iface != IfaceMQTT {
		t.Fatalf("path via mqtt: %+v", e)
	}
}

// The legacy layer trusts what it hears the way it trusted the stunnel leg,
// where every peer holds a Hub-issued client certificate: it answers time
// sync, signs custody ACKs and forwards onto the kits and the satellites. So
// the same MeshSat-private frames that reach it from the bridge listener are
// dropped and counted when a stranger sends them to the public listener
// (MESHSAT-1720).
func TestService_PublicFramesNeverReachTheLegacyLayer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var declined [][]byte
	var dmu sync.Mutex
	nDeclined := func() int {
		dmu.Lock()
		defer dmu.Unlock()
		return len(declined)
	}
	svc := newService(t, nil, nil, &declined)
	svc.cfg.OnDeclinedTCP = func(raw []byte) {
		dmu.Lock()
		defer dmu.Unlock()
		declined = append(declined, append([]byte(nil), raw...))
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()

	// fill differs per listener: the node drops a frame whose hash it has
	// already seen, so the same bytes twice would not test the second leg.
	frame := func(first, fill byte) []byte {
		f := make([]byte, rnswire.HeaderMinSize+8)
		f[0] = first
		for i := 2; i < len(f); i++ {
			f[i] = fill
		}
		return f
	}
	send := func(addr string, fill byte) {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		for _, first := range []byte{
			0x02, // HEADER_1 DATA to a plain destination: forwarded by the legacy relay
			0x14, // the legacy time sync request
			0x16, // the legacy custody offer
			0x17, // the legacy custody ACK
		} {
			if _, err := conn.Write(rnswire.HDLCFrame(frame(first, fill))); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The control first: what the node declines from the bridge listener
	// reaches the legacy handler, so these frames are the legacy layer's.
	send(svc.Status().Bridge.Addr, 0x5a)
	waitFor(t, "bridge frames declined to the legacy handler", func() bool { return nDeclined() >= 1 })
	time.Sleep(200 * time.Millisecond) // let the rest of the burst land
	k := nDeclined()
	dmu.Lock()
	first := declined[0][0]
	dmu.Unlock()
	if first != 0x02 {
		t.Fatalf("the forwardable legacy frame did not reach the legacy handler first: %#x", first)
	}
	if svc.Status().PublicDropped != 0 {
		t.Fatal("bridge frames counted as public drops")
	}

	// The same frames from a stranger: every one the node declines is
	// dropped and counted, none reaches the legacy handler.
	send(svc.Status().Public.Addr, 0xa5)
	waitFor(t, "public frames dropped", func() bool { return svc.Status().PublicDropped == uint64(k) })
	time.Sleep(200 * time.Millisecond)
	if n := nDeclined(); n != k {
		t.Fatalf("%d frames from the public listener reached the legacy handler", n-k)
	}
}

// An announce heard on the public listener is the node's business only: it
// learns the path, but the legacy router, which feeds the relay to the kits
// and the satellites, never hears of it. One on the bridge listener is
// mirrored as before.
func TestService_PublicAnnouncesAreNotMirroredToTheLegacyRouter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mmu sync.Mutex
	mirrored := map[string]Source{}
	var declined [][]byte
	svc := newService(t, nil, nil, &declined)
	svc.cfg.OnAnnounce = func(ann *rnswire.Announce, _ []byte, src Source) {
		mmu.Lock()
		defer mmu.Unlock()
		mirrored[hex.EncodeToString(ann.DestHash[:])] = src
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()

	public := dialPeer(t, ctx, svc.Status().Public.Addr)
	bridge := dialPeer(t, ctx, svc.Status().Bridge.Addr)
	pdest := public.node.AddDestination(&rns.Destination{Name: "test.public"})
	bdest := bridge.node.AddDestination(&rns.Destination{Name: "test.bridge"})
	waitFor(t, "both connected", func() bool {
		st := svc.Status()
		return len(st.Public.Connections) == 1 && len(st.Bridge.Connections) == 1
	})
	if err := public.node.Announce(pdest, "", false); err != nil {
		t.Fatal(err)
	}
	if err := bridge.node.Announce(bdest, "", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both paths learned by the node", func() bool {
		n := svc.Node()
		return n != nil && n.Paths().Has(pdest.Hash) && n.Paths().Has(bdest.Hash)
	})
	waitFor(t, "bridge announce mirrored", func() bool {
		mmu.Lock()
		defer mmu.Unlock()
		return mirrored[hex.EncodeToString(bdest.Hash[:])] == SourceTCP
	})
	mmu.Lock()
	defer mmu.Unlock()
	if src, ok := mirrored[hex.EncodeToString(pdest.Hash[:])]; ok {
		t.Fatalf("public announce mirrored to the legacy router as %q", src)
	}
}
