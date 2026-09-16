// Package mesh records which Meshtastic nodes the Hub has heard behind which
// bridge, so something other than guesswork can answer "is anything reachable
// out there?" (MESHSAT-1181).
//
// What this can prove, and what it cannot, is the whole design:
//
//   - A recorded node IS proof that it transmitted behind that bridge then.
//   - Nothing recorded proves NOTHING. A node that is powered on, in range and
//     simply not talking looks exactly like a node that is absent.
//
// Only mo/decoded carries a bridge_id -- position and telemetry do not -- so a
// node becomes visible when a human sends text through it and not before. At
// the start of a show day the table is legitimately empty while both meshes are
// perfectly fine. Callers must therefore treat presence as a positive signal to
// act on, and silence as "unknown", never as "empty".
package mesh

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// Recorder notes mesh node presence from the traffic the Hub already receives.
type Recorder struct {
	bus     bus.MessageBus
	store   store.Store
	tenants *tenancy.Resolver
	now     func() time.Time
}

// NewRecorder builds the presence recorder.
func NewRecorder(b bus.MessageBus, s store.Store, tenants *tenancy.Resolver) *Recorder {
	if tenants == nil {
		tenants = tenancy.NewResolver(s, store.DefaultTenantID, 30*time.Second)
	}
	return &Recorder{bus: b, store: s, tenants: tenants, now: time.Now}
}

// Start subscribes to decoded MO traffic.
//
// Both replicas record every message and that is deliberate: this is an upsert
// of an observation, not a side effect, so there is nothing to claim and a
// double write is simply the same fact written twice. Compare the booth's
// sends, which DO claim -- the test is whether repeating it costs money or
// speaks to a person.
func (r *Recorder) Start() error {
	// Written literally rather than through TopicMODecoded("+"), which would
	// percent-encode the wildcard and match nothing (Critical Rule 18).
	for _, f := range hubmqtt.DualFilters("meshsat/+/mo/decoded") {
		if err := r.bus.Subscribe(f, 1, r.handle); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) handle(topic string, payload []byte) {
	var m struct {
		DeviceID string `json:"device_id"`
		BridgeID string `json:"bridge_id"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return
	}
	// No bridge means the message did not come off a mesh -- a satellite or SMS
	// device reaches the Hub without one, and neither says anything about radio
	// reachability behind a kit.
	if m.BridgeID == "" {
		return
	}
	nodeID := m.DeviceID
	if nodeID == "" {
		nodeID = hubmqtt.ExtractDeviceID(topic)
	}
	if nodeID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tenantID := r.tenants.ForBridgeTopic(ctx, m.BridgeID, hubmqtt.ExtractTenantID(topic))
	if err := r.store.RecordMeshNode(ctx, tenantID, m.BridgeID, nodeID, r.now()); err != nil {
		slog.Warn("mesh: could not record node presence",
			"bridge", m.BridgeID, "node", nodeID, "error", err)
		return
	}
	slog.Debug("mesh: node heard", "bridge", m.BridgeID, "node", nodeID)
}

// LiveFunc answers whether a node has been heard behind bridgeID inside window.
//
// A store error answers TRUE, not false. This gates a booth menu: a database
// blip must not take a working mesh out of the demo, and the downstream cost of
// a wrong "yes" is one unanswered message that the expiry sweep already
// explains, against a wrong "no" that hides a mesh nobody can then use.
func LiveFunc(s store.Store, window time.Duration) func(ctx context.Context, tenantID, bridgeID string) bool {
	return func(ctx context.Context, tenantID, bridgeID string) bool {
		nodes, err := s.MeshNodesSeenSince(ctx, tenantID, bridgeID, time.Now().Add(-window))
		if err != nil {
			slog.Warn("mesh: presence lookup failed, treating the mesh as available",
				"bridge", bridgeID, "error", err)
			return true
		}
		return len(nodes) > 0
	}
}
