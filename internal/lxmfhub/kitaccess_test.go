package lxmfhub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The kits' joins (MESHSAT-1715): what a kit is told is the stored row, only a
// bridge the store files under the topic's tenant is answered, only by the
// replica whose node runs, at QoS 0 and never retained.

type kitPub struct {
	mu   sync.Mutex
	sent []kitSent
}

type kitSent struct {
	topic    string
	qos      byte
	retained bool
	access   KitAccess
}

func (p *kitPub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	var a KitAccess
	_ = json.Unmarshal(payload, &a)
	p.mu.Lock()
	p.sent = append(p.sent, kitSent{topic, qos, retained, a})
	p.mu.Unlock()
	return nil
}

func (p *kitPub) on(topic string) []kitSent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []kitSent
	for _, s := range p.sent {
		if s.topic == topic {
			out = append(out, s)
		}
	}
	return out
}

type kitBridges map[string][]string // tenant -> bridge ids

func (k kitBridges) GetBridge(_ context.Context, tenantID, bridgeID string) (*store.Bridge, error) {
	for _, id := range k[tenantID] {
		if id == bridgeID {
			return &store.Bridge{TenantID: tenantID, BridgeID: bridgeID}, nil
		}
	}
	return nil, store.ErrNotFound
}

func (k kitBridges) ListBridges(_ context.Context, tenantID string) ([]*store.Bridge, error) {
	var out []*store.Bridge
	for _, id := range k[tenantID] {
		out = append(out, &store.Bridge{TenantID: tenantID, BridgeID: id})
	}
	return out, nil
}

const defaultAccessTopic = "meshsat/bridge/kit1/rns/access"

func waitSent(t *testing.T, p *kitPub, topic string, n int) []kitSent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.on(topic); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d sent on %s, want %d", len(p.on(topic)), topic, n)
	return nil
}

func quiet(t *testing.T, p *kitPub, topic string, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := p.on(topic); len(got) != n {
		t.Fatalf("%d sent on %s, want %d: %+v", len(got), topic, n, got)
	}
}

func TestKitJoin_TheStoredCodeGoesToARegisteredBridgeOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	id, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	pub := &kitPub{}
	if err := f.svc.SetKitAccess(pub, kitBridges{store.DefaultTenantID: {"kit1"}, "other": {"kit2"}}, "rns.example.org:4242"); err != nil {
		t.Fatal(err)
	}
	f.svc.HandleJoin("meshsat/bridge/kit1/rns/join", []byte(`{"v":1}`))
	got := waitSent(t, pub, defaultAccessTopic, 1)[0]
	if got.qos != 0 || got.retained {
		t.Fatalf("sent at QoS %d retained %v; the passphrase must not be queued or kept by the broker", got.qos, got.retained)
	}
	want := KitAccess{V: 1, Mode: KitModePrivate, Host: "rns.example.org", Port: 4242, IFACNetname: id.IFACNetname, IFACPassphrase: id.IFACPassphrase}
	if got.access != want || want.IFACPassphrase == "" {
		t.Fatalf("told %+v, want %+v", got.access, want)
	}
	// Again within the limit: not answered.
	f.svc.HandleJoin("meshsat/bridge/kit1/rns/join", nil)
	quiet(t, pub, defaultAccessTopic, 1)
	// kit2 is the other tenant's: asking in the default namespace gets nothing,
	// and neither does an id nobody registered, nor kit1 in another namespace.
	f.svc.HandleJoin("meshsat/bridge/kit2/rns/join", nil)
	f.svc.HandleJoin("meshsat/bridge/ghost/rns/join", nil)
	f.svc.HandleJoin("meshsat/other/bridge/kit1/rns/join", nil)
	time.Sleep(200 * time.Millisecond)
	pub.mu.Lock()
	n := len(pub.sent)
	pub.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d sent in all, want only the first answer", n)
	}
}

func TestKitJoin_AReplicaWithoutTheNodeSaysNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	if _, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org"); err != nil {
		t.Fatal(err)
	}
	// The other replica: the same rows, no node.
	other := New(Config{}, f.st, f.acc, nil)
	pub := &kitPub{}
	if err := other.SetKitAccess(pub, kitBridges{store.DefaultTenantID: {"kit1"}}, "rns.example.org:4242"); err != nil {
		t.Fatal(err)
	}
	other.HandleJoin("meshsat/bridge/kit1/rns/join", nil)
	other.Reload(store.DefaultTenantID)
	quiet(t, pub, defaultAccessTopic, 0)
}

// A mode change and a rotation reach every bridge of the tenant at once, the
// newest row last; Public and Off say none.
func TestKitPush_ARowChangeTellsEveryBridgeTheNewestRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	pub := &kitPub{}
	if err := f.svc.SetKitAccess(pub, kitBridges{store.DefaultTenantID: {"kit1", "kit3"}, "other": {"kit2"}}, "rns.example.org:4242"); err != nil {
		t.Fatal(err)
	}
	id, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	waitSent(t, pub, defaultAccessTopic, 1)
	waitSent(t, pub, "meshsat/bridge/kit3/rns/access", 1)
	public := AccessPublic
	if _, err := f.svc.UpdateIdentity(ctx, store.DefaultTenantID, "owner@example.org", IdentityUpdate{AccessMode: &public}); err != nil {
		t.Fatal(err)
	}
	if got := waitSent(t, pub, defaultAccessTopic, 2); got[len(got)-1].access.Mode != KitModeNone || got[len(got)-1].access.IFACPassphrase != "" {
		t.Fatalf("public: told %+v", got[len(got)-1].access)
	}
	private := AccessPrivate
	if _, err := f.svc.UpdateIdentity(ctx, store.DefaultTenantID, "owner@example.org", IdentityUpdate{AccessMode: &private}); err != nil {
		t.Fatal(err)
	}
	rot, err := f.svc.RotateAccess(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	got := waitSent(t, pub, defaultAccessTopic, 4)
	last := got[len(got)-1].access
	if last.Mode != KitModePrivate || last.IFACNetname != rot.IFACNetname || last.IFACPassphrase != rot.IFACPassphrase || last.IFACPassphrase == id.IFACPassphrase {
		t.Fatalf("after the rotation the last word is %+v", last)
	}
	// The same row again (another provider's edit) is not pushed again.
	f.svc.Reload(store.DefaultTenantID)
	quiet(t, pub, defaultAccessTopic, 4)
	// The other tenant's bridge heard none of it.
	quiet(t, pub, "meshsat/other/bridge/kit2/rns/access", 0)
}

// What a kit is told is the stored row, not whether the address is
// registered: a Private node that is not there must not make every kit leave.
func TestKitJoin_TheRowDecidesNotTheRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newAccessFixture(t, ctx)
	f.svc.SetNodes(nil)
	pub := &kitPub{}
	if err := f.svc.SetKitAccess(pub, kitBridges{store.DefaultTenantID: {"kit1"}}, "rns.example.org:4242"); err != nil {
		t.Fatal(err)
	}
	id, err := f.svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	f.svc.mu.Lock()
	registered := f.svc.routers[store.DefaultTenantID] != nil
	f.svc.mu.Unlock()
	if registered {
		t.Fatal("the fixture registered the address; the test needs it unregistered")
	}
	got := waitSent(t, pub, defaultAccessTopic, 1)
	if a := got[0].access; a.Mode != KitModePrivate || a.IFACPassphrase != id.IFACPassphrase {
		t.Fatalf("told %+v with the address unregistered, want the stored code", a)
	}
}

func TestSetKitAccessRefusesABadEndpoint(t *testing.T) {
	svc := New(Config{}, nil, nil, nil)
	for _, e := range []string{"", "rns.example.org", ":4242", "rns.example.org:0", "rns.example.org:x"} {
		if err := svc.SetKitAccess(&kitPub{}, kitBridges{}, e); err == nil {
			t.Errorf("%q accepted", e)
		}
	}
}
