package routing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// TestPlainTextSMSRelaysKitToKit is the TTC booth lane "SMS via the Hub"
// (MESHSAT-1022): the SMS webhook publishes an sms.InboundSMS to the sender's
// mo/decoded topic, the engine matches the sms -> sms route whose senders
// list names the origin kit, and the payload the SMS destination reads
// carries the text under the "text" key so the relay is "[origin] text" and
// not an empty prefix. The reverse origin must not fire the route.
func TestPlainTextSMSRelaysKitToKit(t *testing.T) {
	const (
		kitA = "+31653618463"
		kitB = "+31653207829"
	)
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoute(ctx, store.DefaultTenantID, &store.Route{
		Name: "TTC: kit A -> kit B over SMS", SourceType: "sms", DestinationType: "sms",
		Filter: kitB, Senders: kitA, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var origins []string
	var texts []string
	e := NewEngine(s, nil, tenancy.NewResolver(s, store.DefaultTenantID, 0))
	e.RegisterHandler("sms", func(_ context.Context, _ *store.Route, deviceID string, payload json.RawMessage) {
		var msg moDecodedPayload
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Errorf("payload: %v", err)
			return
		}
		mu.Lock()
		origins = append(origins, deviceID)
		texts = append(texts, formatRoutedSMS(deviceID, msg.Text))
		mu.Unlock()
	})

	publish := func(from, id, text string) {
		payload, err := json.Marshal(sms.InboundSMS{ID: id, From: from, To: "+3197010258258", Body: text, Text: text, Channel: "sms"})
		if err != nil {
			t.Fatal(err)
		}
		e.handleMODecoded("meshsat/"+from+"/mo/decoded", payload)
	}
	publish(kitA, "sms-in-SM1", "hello from A")
	publish(kitB, "sms-in-SM2", "hello from B") // not in senders: must not fire
	publish(kitA, "sms-in-SM1", "hello from A") // Twilio retry: same id, claimed once

	mu.Lock()
	defer mu.Unlock()
	if len(origins) != 1 || origins[0] != kitA {
		t.Fatalf("route fired for origins %v, want exactly [%s]", origins, kitA)
	}
	if want := "[" + kitA + "] hello from A"; texts[0] != want {
		t.Errorf("relayed text = %q, want %q", texts[0], want)
	}
}
