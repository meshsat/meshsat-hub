package bridge

import (
	"context"
	"encoding/json"
	"github.com/meshsat/meshsat-hub/internal/store"
	"strings"
	"testing"
	"time"
)

type fakeUplinkStore struct {
	health     map[string]string
	lastSeen   int
	reportB    string
	reportAt   time.Time
	reportCall int
}

func (f *fakeUplinkStore) SetBridgeHealth(_ context.Context, _ string, id string, h string) error {
	if f.health == nil {
		f.health = map[string]string{}
	}
	f.health[id] = h
	return nil
}
func (f *fakeUplinkStore) RecordBridgeHealth(context.Context, string, string, string, string, time.Time) error {
	return nil
}

func (f *fakeUplinkStore) TouchBridgeLastSeen(context.Context, string, string) error {
	f.lastSeen++
	return nil
}
func (f *fakeUplinkStore) SetBridgeLastReport(_ context.Context, _ string, _ string, bearer string, at time.Time) error {
	f.reportB, f.reportAt, f.reportCall = bearer, at, f.reportCall+1
	return nil
}

type pubRec struct {
	topic    string
	retained bool
	body     map[string]any
}

// Position, SOS and health frames over any bearer become fleet state: the
// position topic, mo/decoded with sos=true (the SOS detector's trigger), the
// bridge's health JSON with per-interface status, and the last report
// bearer/time (MESHSAT-964 B).
func TestUplinkSink_Frames(t *testing.T) {
	st := &fakeUplinkStore{}
	var pubs []pubRec
	sink := NewUplinkSink(st, func(topic string, _ byte, retained bool, v any) {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		pubs = append(pubs, pubRec{topic: topic, retained: retained, body: m})
	}, nil, "test")
	fixed := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sink.now = func() time.Time { return fixed }
	ctx := context.Background()

	if sink.Handle(ctx, "t1", "sms", "+3160000", []byte("hello")) {
		t.Fatalf("plain text treated as uplink")
	}

	ts := fixed.Add(-2 * time.Minute)
	if !sink.Handle(ctx, "t1", "sms", "+3160000", encodeSatPosition("tesseract", 52.1, 4.3, 12, 1, ts)) {
		t.Fatalf("position not handled")
	}
	if len(pubs) != 1 || pubs[0].topic != "meshsat/t1/tesseract/position" || !pubs[0].retained || pubs[0].body["source"] != "sms_uplink" {
		t.Fatalf("position publish: %+v", pubs)
	}
	if st.reportB != "sms" || !st.reportAt.Equal(ts) || st.lastSeen != 1 {
		t.Errorf("report after position: %s %v %d", st.reportB, st.reportAt, st.lastSeen)
	}

	pubs = nil
	if !sink.Handle(ctx, "t1", "sbd", "300234065000001", encodeSatSOS("tesseract", "dev-7", 52.1, 4.3, "man overboard", ts)) {
		t.Fatalf("SOS not handled")
	}
	if len(pubs) != 2 || pubs[0].topic != "meshsat/t1/tesseract/sos" || pubs[1].topic != "meshsat/t1/dev-7/mo/decoded" {
		t.Fatalf("SOS publishes: %+v", pubs)
	}
	if pubs[1].body["sos"] != true || pubs[1].body["text"] != "man overboard" || pubs[1].body["channel"] != "sbd" || !strings.HasPrefix(pubs[1].body["id"].(string), "sos-sbd-tesseract-") {
		t.Errorf("SOS mo/decoded body: %+v", pubs[1].body)
	}
	if st.reportB != "sbd" {
		t.Errorf("report after SOS: %s", st.reportB)
	}

	pubs = nil
	ifaces := []SatIfaceStatus{{Name: "iridium_0", Online: true, Signal: 4}, {Name: "cellular_0", Online: false, Signal: 0}}
	if !sink.Handle(ctx, "t1", "imt", "300234065000002", encodeSatHealth("parallax", 3600, 12, 40, 55, ifaces, ts)) {
		t.Fatalf("health not handled")
	}
	var h map[string]any
	if err := json.Unmarshal([]byte(st.health["parallax"]), &h); err != nil {
		t.Fatalf("health JSON: %v", err)
	}
	list, _ := h["interfaces"].([]any)
	if len(list) != 2 || h["source"] != "imt_uplink" || h["bearer"] != "imt" {
		t.Fatalf("health: %+v", h)
	}
	first := list[0].(map[string]any)
	second := list[1].(map[string]any)
	if first["name"] != "iridium_0" || first["status"] != "up" || first["signal_bars"] != float64(4) || second["status"] != "down" {
		t.Errorf("interfaces: %+v", list)
	}
	if len(pubs) != 0 || st.reportB != "imt" || st.reportCall != 3 {
		t.Errorf("health side effects: pubs=%d bearer=%s calls=%d", len(pubs), st.reportB, st.reportCall)
	}

	// A frame with a nonsense timestamp records the time of receipt.
	_ = sink.Handle(ctx, "t1", "sms", "+3160000", encodeSatPosition("tesseract", 52.1, 4.3, 12, 1, fixed.Add(48*time.Hour)))
	if !st.reportAt.Equal(fixed) {
		t.Errorf("future timestamp not clamped: %v", st.reportAt)
	}
}

// listingStore is a fakeUplinkStore that also knows which bridges a tenant has.
type listingStore struct {
	fakeUplinkStore
	byTenant map[string][]string
}

func (l *listingStore) ListBridges(_ context.Context, tenantID string) ([]*store.Bridge, error) {
	var out []*store.Bridge
	for _, id := range l.byTenant[tenantID] {
		out = append(out, &store.Bridge{BridgeID: id})
	}
	return out, nil
}

// The field kits' encoder cut the bridge id at 16 bytes. The health update then
// ran against a bridge that does not exist: zero rows, no error, no log line
// (seen live, 2026-09-21). A shortened id is matched to the ONE bridge of the
// same tenant it fits, and never to another tenant's.
func TestUplinkRecognisesAShortenedBridgeID(t *testing.T) {
	ts := time.Date(2026, 9, 21, 4, 5, 0, 0, time.UTC)
	frame := func(id string) []byte { return encodeSatHealth(id, 5311, 1, 14, 28, nil, ts) }
	newSink := func(st *listingStore) *UplinkSink {
		s := NewUplinkSink(st, func(string, byte, bool, any) {}, nil, "test")
		s.now = func() time.Time { return ts }
		return s
	}

	st := &listingStore{byTenant: map[string][]string{
		"t1": {"bridge-kit-alpha-01", "bridge-kit-bravo-01"},
		"t2": {"bridge-kit-alpha-99"},
	}}
	newSink(st).Handle(context.Background(), "t1", "sms", "+31600000001", frame("bridge-kit-alpha")) // 16 bytes
	if _, ok := st.health["bridge-kit-alpha-01"]; !ok {
		t.Fatalf("the shortened id did not reach the bridge it fits; health written for: %v", st.health)
	}
	if _, ok := st.health["bridge-kit-alpha"]; ok {
		t.Error("health was also written under the shortened id, a bridge that does not exist")
	}

	// Ambiguous within the tenant: nothing is guessed.
	amb := &listingStore{byTenant: map[string][]string{"t1": {"bridge-kit-alpha-01", "bridge-kit-alpha-02"}}}
	newSink(amb).Handle(context.Background(), "t1", "sms", "+31600000001", frame("bridge-kit-alpha"))
	if _, ok := amb.health["bridge-kit-alpha-01"]; ok {
		t.Error("an id that fits two bridges was given to one of them")
	}

	// Another tenant's bridge is never a candidate, however well the id fits.
	other := &listingStore{byTenant: map[string][]string{"t1": {"something-else"}, "t2": {"bridge-kit-alpha-99"}}}
	newSink(other).Handle(context.Background(), "t1", "sms", "+31600000001", frame("bridge-kit-alpha"))
	if _, ok := other.health["bridge-kit-alpha-99"]; ok {
		t.Fatal("a frame carried by tenant t1 updated tenant t2's bridge")
	}

	// An exact id is untouched by any of this.
	exact := &listingStore{byTenant: map[string][]string{"t1": {"bridge-kit-alpha", "bridge-kit-alpha-01"}}}
	newSink(exact).Handle(context.Background(), "t1", "sms", "+31600000001", frame("bridge-kit-alpha"))
	if _, ok := exact.health["bridge-kit-alpha"]; !ok {
		t.Error("an exact match lost to a longer id that starts with it")
	}
}

// An SOS the bridge raised itself names its device "bridge". The alert, and the
// page the on-call person reads, must name the kit instead: "[sos] bridge: ..."
// cannot tell two kits apart (MESHSAT-1294). A node's SOS relayed by the bridge
// keeps the node's id.
func TestABridgeRaisedSOSIsFiledUnderTheBridge(t *testing.T) {
	st := &fakeUplinkStore{}
	var pubs []pubRec
	sink := NewUplinkSink(st, func(topic string, _ byte, retained bool, v any) {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		pubs = append(pubs, pubRec{topic: topic, retained: retained, body: m})
	}, nil, "test")
	fixed := time.Date(2026, 9, 21, 8, 25, 28, 0, time.UTC)
	sink.now = func() time.Time { return fixed }
	ctx := context.Background()

	for _, tc := range []struct{ device, want string }{
		{"bridge", "kit-alpha"},    // raised on the kit itself
		{"", "kit-alpha"},          // no device at all
		{"!0a1b2c3d", "!0a1b2c3d"}, // a mesh node's SOS, relayed
	} {
		pubs = nil
		frame := encodeSatSOS("kit-alpha", tc.device, 0, 0, "SOS - EMERGENCY ALERT", fixed.Add(-4*time.Second))
		if !sink.Handle(ctx, "t1", "sms", "+3160000", frame) {
			t.Fatalf("device %q: SOS not handled", tc.device)
		}
		if len(pubs) != 2 {
			t.Fatalf("device %q: publishes %+v", tc.device, pubs)
		}
		mo := pubs[1]
		if mo.body["imei"] != tc.want || !strings.HasSuffix(mo.topic, "/mo/decoded") || mo.body["bridge_id"] != "kit-alpha" {
			t.Errorf("device %q: filed under %v on %s, want %s", tc.device, mo.body["imei"], mo.topic, tc.want)
		}
	}
}
