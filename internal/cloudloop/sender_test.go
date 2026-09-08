package cloudloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

type recordingBus struct {
	mu     sync.Mutex
	topics []string
}

func (b *recordingBus) Connect() error { return nil }
func (b *recordingBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
	return nil
}
func (b *recordingBus) PublishJSON(topic string, qos byte, retained bool, _ any) error {
	return b.Publish(topic, qos, retained, nil)
}
func (b *recordingBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *recordingBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (b *recordingBus) IsConnected() bool                                             { return true }
func (b *recordingBus) Disconnect()                                                   {}

type staticResolver struct{ thing string }

func (r staticResolver) Resolve(string) (string, bool) { return r.thing, false }

type memCosts struct {
	mu      sync.Mutex
	entries []*CostEntry
	tenants []string
}

func (m *memCosts) InsertCostEntry(_ context.Context, tenantID string, c *CostEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, c)
	m.tenants = append(m.tenants, tenantID)
	return nil
}

// A cost row and the mt/status topic are keyed by the device IMEI even when the
// Cloudloop call goes out under the resolved thing ID (the Costs page showed
// thing IDs for every MT send before this was pinned, 2026-09-08).
func TestSendDirect_CostAndStatusUseIMEI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("thing"); got != "THING-abc" {
			t.Errorf("thing = %q, want THING-abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "sbd-1", Status: "queued"})
	}))
	defer server.Close()

	rb := &recordingBus{}
	costs := &memCosts{}
	s := NewSender(NewClient(server.URL, "k"), rb)
	s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	s.SetCostRecorder(costs)
	s.SetCostPerMessage(0.05)

	res, err := s.SendDirect("300434065000001", MTSendRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if res.ThingID != "THING-abc" {
		t.Errorf("ThingID = %q", res.ThingID)
	}
	if len(costs.entries) != 1 {
		t.Fatalf("cost entries = %d, want 1", len(costs.entries))
	}
	if costs.entries[0].DeviceIMEI != "300434065000001" {
		t.Errorf("cost DeviceIMEI = %q, want the IMEI", costs.entries[0].DeviceIMEI)
	}
	if costs.tenants[0] != "default" {
		t.Errorf("cost tenant = %q, want default", costs.tenants[0])
	}
	if len(rb.topics) != 1 || rb.topics[0] != "meshsat/300434065000001/mt/status" {
		t.Errorf("status topics = %v, want meshsat/300434065000001/mt/status", rb.topics)
	}
}
