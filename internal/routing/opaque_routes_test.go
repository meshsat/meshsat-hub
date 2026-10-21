package routing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// What the satellite webhooks publish decides where a message may go: an
// opaque one (a declared modem's ciphertext, base64 ciphertext, a Reticulum
// packet, anything unreadable) reaches only destinations that carry an
// opaque payload; a readable one (plain SMAZ2 included, R26-R1) reaches
// destinations that show text as well.
func TestOpaqueMessagesReachOnlyOpaqueDestinations(t *testing.T) {
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*store.Route{
		{Name: "to a person", SourceType: "*", DestinationType: "sms", Filter: "+31600000000", Enabled: true},
		{Name: "to the other kit", SourceType: "*", DestinationType: DestSatelliteRelay, Filter: "300434060000002", Enabled: true},
		{Name: "to a webhook", SourceType: "*", DestinationType: "webhook", Enabled: true},
	} {
		if err := s.CreateRoute(ctx, store.DefaultTenantID, r); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	fired := map[string][]string{}
	e := NewEngine(s, nil, nil)
	for _, d := range []string{"sms", DestSatelliteRelay, "webhook"} {
		d := d
		e.RegisterHandler(d, func(_ context.Context, _ *store.Route, deviceID string, raw json.RawMessage) {
			var m struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &m)
			mu.Lock()
			fired[m.ID] = append(fired[m.ID], d)
			mu.Unlock()
		})
	}
	topic := "meshsat/300434060000001/mo/decoded"
	e.handleMODecoded(topic, []byte(`{"id":"mo-readable","imei":"300434060000001","channel":"iridium","text":"Team at the north gate, all safe, moving to camp","compressed":true}`))
	e.handleMODecoded(topic, []byte(`{"id":"mo-sealed","imei":"300434060000001","channel":"iridium","text":"","opaque":true,"wire":"AQID"}`))
	want := map[string]map[string]bool{
		"mo-readable": {"sms": true, DestSatelliteRelay: true, "webhook": true},
		"mo-sealed":   {DestSatelliteRelay: true, "webhook": true},
	}
	for id, dests := range want {
		got := map[string]bool{}
		for _, d := range fired[id] {
			got[d] = true
		}
		if len(got) != len(dests) {
			t.Fatalf("%s reached %v, want %v", id, fired[id], dests)
		}
		for d := range dests {
			if !got[d] {
				t.Fatalf("%s reached %v, want %v", id, fired[id], dests)
			}
		}
	}
}
