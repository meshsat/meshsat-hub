package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type recorded struct {
	tenantID, bridgeID, nodeID string
	at                         time.Time
}

type fakeStore struct {
	store.Store
	got      []recorded
	nodes    []store.MeshNode
	readErr  error
	tenantOf func(bridgeID string) (string, error)
}

func (f *fakeStore) RecordMeshNode(_ context.Context, tenantID, bridgeID, nodeID string, at time.Time) error {
	f.got = append(f.got, recorded{tenantID, bridgeID, nodeID, at})
	return nil
}

func (f *fakeStore) MeshNodesSeenSince(context.Context, string, string, time.Time) ([]store.MeshNode, error) {
	return f.nodes, f.readErr
}

// The recorder resolves the owning tenant from the bridge, so the double has to
// answer that too. Both booth kits belong to the platform tenant.
func (f *fakeStore) LookupBridgeTenant(_ context.Context, bridgeID string) (string, error) {
	if f.tenantOf != nil {
		return f.tenantOf(bridgeID)
	}
	return store.DefaultTenantID, nil
}

type fakeBus struct {
	bus.MessageBus
	handlers map[string]bus.MessageHandler
}

func (b *fakeBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	if b.handlers == nil {
		b.handlers = map[string]bus.MessageHandler{}
	}
	b.handlers[topic] = h
	return nil
}

// deliver hands a payload to the first subscribed handler, as the broker would.
func (b *fakeBus) deliver(topic string, payload []byte) {
	for _, h := range b.handlers {
		h(topic, payload)
		return
	}
}

func newRecorder(t *testing.T) (*Recorder, *fakeBus, *fakeStore) {
	t.Helper()
	s := &fakeStore{}
	b := &fakeBus{}
	r := NewRecorder(b, s, nil)
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(b.handlers) == 0 {
		t.Fatal("recorder subscribed to nothing")
	}
	return r, b, s
}

// A mesh message names its bridge, and that is what makes presence knowable at
// all: the topic carries only the node.
func TestAMeshMessageRecordsItsNodeBehindItsBridge(t *testing.T) {
	_, b, s := newRecorder(t)
	b.deliver("meshsat/!0a0b0c0d/mo/decoded",
		[]byte(`{"device_id":"!0a0b0c0d","bridge_id":"bridge-kit-a","text":"hello"}`))

	if len(s.got) != 1 {
		t.Fatalf("recorded %d observations, want 1", len(s.got))
	}
	if s.got[0].bridgeID != "bridge-kit-a" || s.got[0].nodeID != "!0a0b0c0d" {
		t.Errorf("recorded the wrong pair: %+v", s.got[0])
	}
}

// Satellite and SMS traffic reaches the same topic with no bridge. Recording it
// would invent a mesh node behind a kit that never heard anything, which is the
// precise lie this table exists to avoid.
func TestTrafficWithNoBridgeIsNotPresence(t *testing.T) {
	_, b, s := newRecorder(t)
	b.deliver("meshsat/%2B31600000001/mo/decoded",
		[]byte(`{"device_id":"+31600000001","text":"an inbound SMS"}`))
	b.deliver("meshsat/300434063281370/mo/decoded",
		[]byte(`{"imei":"300434063281370","text":"a satellite message"}`))

	if len(s.got) != 0 {
		t.Fatalf("recorded presence for traffic that never touched a mesh: %+v", s.got)
	}
}

// Malformed JSON must not take the subscriber down or write a phantom node.
func TestGarbageIsIgnored(t *testing.T) {
	_, b, s := newRecorder(t)
	b.deliver("meshsat/!0a0b0c0d/mo/decoded", []byte(`{not json`))
	b.deliver("meshsat/!0a0b0c0d/mo/decoded", []byte(`{"bridge_id":"","device_id":""}`))
	if len(s.got) != 0 {
		t.Fatalf("recorded something from garbage: %+v", s.got)
	}
}

// When the payload omits device_id, the topic still names the node.
func TestNodeFallsBackToTheTopic(t *testing.T) {
	_, b, s := newRecorder(t)
	b.deliver("meshsat/!a1b3c3a4/mo/decoded", []byte(`{"bridge_id":"bridge-kit-b","text":"hi"}`))
	if len(s.got) != 1 || s.got[0].nodeID != "!a1b3c3a4" {
		t.Fatalf("did not fall back to the topic for the node id: %+v", s.got)
	}
}

// LiveFunc is a booth menu gate. A database blip must not take a working mesh
// out of the demo, so a read error answers "available" -- the cost of a wrong
// yes is one unanswered message the expiry sweep already explains, against a
// wrong no that hides a mesh nobody can then use.
func TestAPresenceLookupFailureLeavesTheMeshAvailable(t *testing.T) {
	s := &fakeStore{readErr: errors.New("connection refused")}
	if !LiveFunc(s, 30*time.Minute)(context.Background(), "t1", "bridge-kit-a") {
		t.Error("a store error hid a mesh from the menu; it must fail open")
	}
}

func TestLiveFuncReportsWhatTheStoreSays(t *testing.T) {
	ctx := context.Background()
	empty := &fakeStore{}
	if LiveFunc(empty, 30*time.Minute)(ctx, "t1", "bridge-kit-a") {
		t.Error("no heard nodes reported as live")
	}
	heard := &fakeStore{nodes: []store.MeshNode{{NodeID: "!0a0b0c0d"}}}
	if !LiveFunc(heard, 30*time.Minute)(ctx, "t1", "bridge-kit-a") {
		t.Error("a heard node was not reported as live")
	}
}
