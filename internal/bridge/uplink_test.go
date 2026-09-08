package bridge

import (
	"context"
	"encoding/json"
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
