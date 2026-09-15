package cloudloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// memClaimer is store.ClaimOnce in a map: the shared claim table both
// replicas consult.
type memClaimer struct {
	mu   sync.Mutex
	seen map[string]bool
	fail bool
}

func (c *memClaimer) ClaimOnce(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return false, context.DeadlineExceeded
	}
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if c.seen[key] {
		return false, nil
	}
	c.seen[key] = true
	return true, nil
}

// Two replicas both subscribe to meshsat/+/mt/send and both receive every
// request. Before MESHSAT-1120 both POSTed it to Cloudloop, and the tenant was
// billed for two satellite messages per request. Exactly one may send.
func TestMTSendIsSentByExactlyOneReplica(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "sbd-1", Status: "queued"})
	}))
	defer server.Close()

	claims := &memClaimer{}
	replica := func() *Sender {
		s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
		s.SetDeviceResolver(staticResolver{thing: "THING-abc"})
		s.SetClaimer(claims)
		return s
	}
	a, b := replica(), replica()

	topic := "meshsat/300434065000001/mt/send"
	payload := []byte(`{"text":"hello"}`)
	a.handleMTSend(topic, payload)
	b.handleMTSend(topic, payload)
	if got := posts.Load(); got != 1 {
		t.Fatalf("the same mt/send reached Cloudloop %d times, want exactly 1 (two replicas, one send)", got)
	}

	// A request that says it is a different request is sent again, even with
	// the same text: that is what request_id is for.
	a.handleMTSend(topic, []byte(`{"request_id":"r-2","text":"hello"}`))
	b.handleMTSend(topic, []byte(`{"request_id":"r-2","text":"hello"}`))
	if got := posts.Load(); got != 2 {
		t.Fatalf("a distinct request_id was sent %d times in total, want 2", got)
	}

	// A claim that cannot be recorded means NOT sending: a satellite message
	// costs the tenant money, and at-most-once is the right failure mode.
	claims.fail = true
	a.handleMTSend(topic, []byte(`{"request_id":"r-3","text":"hello"}`))
	if got := posts.Load(); got != 2 {
		t.Fatalf("a request was sent while the claim table was unavailable (%d sends)", got)
	}

	// No claimer configured (a single replica) sends every time.
	single := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	single.SetDeviceResolver(staticResolver{thing: "THING-abc"})
	single.handleMTSend(topic, payload)
	if got := posts.Load(); got != 3 {
		t.Fatalf("a sender without a claimer did not send (%d)", got)
	}
}
