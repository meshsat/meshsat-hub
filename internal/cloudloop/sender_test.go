package cloudloop

import (
	"bytes"
	"context"
	"encoding/base64"
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

func (r staticResolver) Resolve(string, string) (string, bool) { return r.thing, false }

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

type imtResolver struct{ thing string }

func (r imtResolver) Resolve(string, string) (string, bool) { return r.thing, true }

// A relayed kit message leaves the Hub as the bytes it arrived as (MESHSAT-1282).
// SendDirect normally turns Text into version byte + text; for a relay that
// would put a second 0x01 in front of the kit's own, and the receiving Bridge
// would strip one, fail base64 on the other and drop the message as
// unauthenticated.
func TestSendDirect_VerbatimWireIsNotTouched(t *testing.T) {
	const wire = "AXN5bnRoZXRpYy1jaXBoZXJ0ZXh0LWJhc2U2NA==" // stands for 0x01 + base64 ciphertext; synthetic
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("message") // Cloudloop takes the payload as base64
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "imt-1", Status: "queued"})
	}))
	defer server.Close()

	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(imtResolver{thing: "THING-kit-b"})

	res, err := s.SendDirect("300000000000002", MTSendRequest{WireB64: wire, Text: "ignored", Compress: true})
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if asked != wire {
		t.Fatalf("Cloudloop was asked to send\n  %q\nwant the wire untouched\n  %q", asked, wire)
	}
	if res.Fragments != 1 || !res.IsIMT {
		t.Errorf("result = %+v, want one IMT frame", res)
	}

	if _, err := s.SendDirect("300000000000002", MTSendRequest{WireB64: "not base64 !!"}); err == nil {
		t.Error("a wire_b64 that is not base64 was accepted")
	}
}

// Verbatim bytes are never cut into this package's fragments: no Bridge
// reassembles them, so the kit would pay for frames it then discards.
func TestSendDirect_VerbatimIsNeverFragmented(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "x", Status: "queued"})
	}))
	defer server.Close()
	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-sbd"}) // an SBD modem: 270-byte frames

	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 400))
	if _, err := s.SendDirect("300434065000001", MTSendRequest{WireB64: big}); err == nil {
		t.Fatal("a 400-byte verbatim payload to an SBD modem was accepted")
	}
	if calls != 0 {
		t.Fatalf("Cloudloop was called %d times for a payload that cannot be sent whole", calls)
	}
}
