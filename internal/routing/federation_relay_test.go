package routing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

func TestFederationRelayHandler(t *testing.T) {
	type call struct{ tenant, peer, from, to, wire, topic, msgID string }
	var mu sync.Mutex
	var calls []call
	h := NewFederationRelayHandler(func(_ context.Context, tenantID, peerID, fromIMEI, toIMEI, wireB64, imtTopic, msgID string) error {
		mu.Lock()
		calls = append(calls, call{tenantID, peerID, fromIMEI, toIMEI, wireB64, imtTopic, msgID})
		mu.Unlock()
		return nil
	})
	route := &store.Route{ID: "r1", Name: "to the peer", Senders: "300434060000001", Filter: "peer-a/300434060000002, nonsense, /300434060000003, peer-b/"}
	payload, _ := json.Marshal(map[string]any{"wire": "AQID", "imt_topic": "IMT_TOPIC_RAW", "channel": "iridium_imt"})
	ctx := tenancy.WithTenant(context.Background(), "t-1")

	h(ctx, route, "300434060000001", payload)
	h(ctx, route, "300434060000001", payload) // a retry of the same message
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("want one relay per handled message to the one valid recipient, got %+v", calls)
	}
	c := calls[0]
	if c.tenant != "t-1" || c.peer != "peer-a" || c.from != "300434060000001" || c.to != "300434060000002" || c.wire != "AQID" || c.topic != "IMT_TOPIC_RAW" {
		t.Fatalf("relay call %+v", c)
	}
	if calls[0].msgID == "" || calls[0].msgID != calls[1].msgID {
		t.Fatalf("msg_id must be stable across a retry: %q %q", calls[0].msgID, calls[1].msgID)
	}

	// Refused outright: no wire payload, a route open to every sender, a
	// dispatch with no tenant.
	calls = nil
	mu.Unlock()
	empty, _ := json.Marshal(map[string]any{"text": "hello", "channel": "iridium_imt"})
	h(ctx, route, "300434060000001", empty)
	open := *route
	open.Senders = ""
	h(ctx, &open, "300434060000001", payload)
	star := *route
	star.Senders = "*"
	h(ctx, &star, "300434060000001", payload)
	h(context.Background(), route, "300434060000001", payload)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(calls) != 0 {
		t.Fatalf("relayed what it should have refused: %+v", calls)
	}

	// The publisher's id is the msg_id when there is one.
	mu.Unlock()
	withID, _ := json.Marshal(map[string]any{"id": "cl-123", "wire": "AQID", "channel": "iridium_imt"})
	h(ctx, route, "300434060000001", withID)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(calls) != 1 || calls[0].msgID != "id-cl-123" {
		t.Fatalf("publisher id not used: %+v", calls)
	}
}
