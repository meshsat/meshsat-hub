package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type memClaimer struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *memClaimer) ClaimOnce(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if c.seen[key] {
		return false, nil
	}
	c.seen[key] = true
	return true, nil
}

// Both replicas receive every meshsat/+/mt/sms; before MESHSAT-1120 both
// texted the phone. Exactly one may.
func TestOutboundSMSIsSentByExactlyOneReplica(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM1","status":"queued"}`))
	}))
	defer srv.Close()

	claims := &memClaimer{}
	replica := func() *Subscriber {
		c := NewClient("ACTEST", "token", "+10000000000")
		c.apiURL = srv.URL
		s := NewSubscriber(c, newInboundMockBus())
		s.SetClaimer(claims)
		return s
	}
	a, b := replica(), replica()
	topic := "meshsat/dev-1/mt/sms"
	payload := []byte(`{"to":"+31600000000","body":"hi"}`)
	a.handle(topic, payload)
	b.handle(topic, payload)
	if got := posts.Load(); got != 1 {
		t.Fatalf("one mt/sms request was texted %d times, want exactly 1", got)
	}
	a.handle(topic, []byte(`{"request_id":"r-2","to":"+31600000000","body":"hi"}`))
	b.handle(topic, []byte(`{"request_id":"r-2","to":"+31600000000","body":"hi"}`))
	if got := posts.Load(); got != 2 {
		t.Fatalf("a distinct request_id was texted %d times in total, want 2", got)
	}
}

// dedupStore is InsertMessage with the real store's contract: a second row
// with the same id is store.ErrDuplicate.
type dedupStore struct {
	store.Store
	mu   sync.Mutex
	rows map[string]*store.Message
}

func (d *dedupStore) InsertMessage(_ context.Context, _ string, m *store.Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rows == nil {
		d.rows = map[string]*store.Message{}
	}
	if _, ok := d.rows[m.ID]; ok {
		return store.ErrDuplicate
	}
	d.rows[m.ID] = m
	return nil
}

// An inbound SMS relayed over MQTT is stored by both replicas. The id used to
// be the processing clock, so every such message was two rows; it is a
// digest of the wire bytes now, and the second insert collides harmlessly.
func TestInboundSMSIsStoredOnceAcrossReplicas(t *testing.T) {
	db := &dedupStore{}
	a := NewInboundSubscriber(newInboundMockBus(), db, "default")
	b := NewInboundSubscriber(newInboundMockBus(), db, "default")
	payload, _ := json.Marshal(InboundMQTTPayload{From: "+31612345678", Body: "Hello", Timestamp: "2026-09-15T00:00:00Z"})
	topic := "meshsat/android-001/sms/inbound"
	a.handleInbound(topic, payload)
	b.handleInbound(topic, payload)
	if len(db.rows) != 1 {
		t.Fatalf("one inbound SMS produced %d rows, want 1", len(db.rows))
	}
	want := "sms-" + hubmqtt.MessageDigest(topic, payload)
	if _, ok := db.rows[want]; !ok {
		t.Fatalf("row id is not the wire digest: have %v", db.rows)
	}
}
