package oob

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// fakeBus is one broker shared by every service that subscribes: Publish
// delivers to all handlers synchronously, including the publisher's own,
// exactly as a plain (non-queue) MQTT subscription does on production.
type fakeBus struct {
	mu       sync.Mutex
	handlers map[string][]bus.MessageHandler
	dropped  bool
}

func newFakeBus() *fakeBus { return &fakeBus{handlers: map[string][]bus.MessageHandler{}} }

func (b *fakeBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	b.handlers[topic] = append(b.handlers[topic], h)
	b.mu.Unlock()
	return nil
}

func (b *fakeBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	hs := append([]bus.MessageHandler(nil), b.handlers[topic]...)
	dropped := b.dropped
	b.mu.Unlock()
	if dropped {
		return nil
	}
	for _, h := range hs {
		h(topic, payload)
	}
	return nil
}

// pairAndSend pairs the kit on svc and starts a Send; it returns the sealed
// request the kit would receive and the channels the reply lands on.
func pairAndSend(t *testing.T, svc *Service, tr *fakeTransport, key []byte, cmd string) (Frame, chan *Reply, chan error) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.Pair(ctx, "t1", "tesseract", key, RoleImporter, "+31653618463", ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan *Reply, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, cmd, ArgSpec{}, false)
		errs <- err
		done <- r
	}()
	wire, _ := Decode(waitForSend(t, tr))
	req, err := Open(wire, key, RoleImporter)
	if err != nil {
		t.Fatal(err)
	}
	return req, done, errs
}

func kitReply(t *testing.T, key []byte, req Frame, counter uint32, seq, total byte, body string) string {
	t.Helper()
	f := Frame{Enc: true, Reply: true, PeerID: req.PeerID, Counter: counter, Cmd: req.Cmd,
		Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: uint16(req.Counter), Seq: seq, Total: total, Body: []byte(body)})}
	rw, err := Seal(f, key, RoleIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return Encode(rw)
}

// Two replicas, one bus. The command leaves replica A; the kit's reply
// arrives through a webhook on replica B (which never sent anything and has
// no waiter); A's caller must still get it. Both replicas share the same
// database, as on production, so B can authenticate the frame.
func TestAReplyOnTheOtherReplicaResolvesTheWaiter(t *testing.T) {
	a, tr, key := newService(t)
	b := New(a.store, a.masterKey, nil, Options{MaxPerHour: 3})
	b.RegisterTransport(BearerSMS, &fakeTransport{timeout: 2 * time.Second})
	shared := newFakeBus()
	if err := a.SetBus(shared); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBus(shared); err != nil {
		t.Fatal(err)
	}
	req, done, errs := pairAndSend(t, a, tr, key, "mgmt_ping")

	if !b.HandleInbound(context.Background(), BearerSMS, "+31653618463", kitReply(t, key, req, 9, 1, 1, "u17h b98A q0")) {
		t.Fatal("reply not classified on replica B")
	}
	if err := <-errs; err != nil {
		t.Fatalf("the waiter on replica A did not get the reply that landed on B: %v", err)
	}
	r := <-done
	if r.Body != "u17h b98A q0" || r.Counter != 1 {
		t.Fatalf("reply: %+v", r)
	}
}

// Without the bus the old behaviour is what the peer session saw on
// production: rc ok logged on B, timeout on A. This pins that the fix IS the
// announcement, not something else in the test rig.
func TestWithoutTheBusAReplyOnTheOtherReplicaIsLost(t *testing.T) {
	a, tr, key := newService(t)
	tr.timeout = 300 * time.Millisecond
	b := New(a.store, a.masterKey, nil, Options{MaxPerHour: 3})
	req, done, errs := pairAndSend(t, a, tr, key, "mgmt_ping")
	b.HandleInbound(context.Background(), BearerSMS, "+31653618463", kitReply(t, key, req, 9, 1, 1, "ok"))
	if err := <-errs; err == nil {
		t.Fatal("a reply on an unconnected replica resolved a waiter elsewhere; the test rig is wrong")
	}
	<-done
}

// A STATUS-NET reply arrives as two SMS segments; the caller gets one reply
// with both bodies in order, whichever order and replica they arrive on
// (seen live 2026-09-15: the API returned "-36", the second segment alone).
func TestSegmentsAreAssembledAcrossReplicasInOrder(t *testing.T) {
	a, tr, key := newService(t)
	b := New(a.store, a.masterKey, nil, Options{MaxPerHour: 3})
	shared := newFakeBus()
	_ = a.SetBus(shared)
	_ = b.SetBus(shared)
	req, done, errs := pairAndSend(t, a, tr, key, "mgmt_status")
	ctx := context.Background()
	// Second segment first, on the other replica; then the first, locally.
	b.HandleInbound(ctx, BearerSMS, "+31653618463", kitReply(t, key, req, 10, 2, 2, " rssi -36"))
	select {
	case err := <-errs:
		t.Fatalf("Send returned on a partial reply: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	a.HandleInbound(ctx, BearerSMS, "+31653618463", kitReply(t, key, req, 11, 1, 2, "u5h b100A q0 wU"))
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.Body != "u5h b100A q0 wU rssi -36" || r.Seq != 2 || r.Total != 2 {
		t.Fatalf("assembled reply: %+v", r)
	}
}

// A partial reply names what is missing instead of a bare timeout.
func TestAPartialReplyTimesOutNamingTheMissingSegments(t *testing.T) {
	a, tr, key := newService(t)
	tr.timeout = 300 * time.Millisecond
	req, done, errs := pairAndSend(t, a, tr, key, "mgmt_status")
	a.HandleInbound(context.Background(), BearerSMS, "+31653618463", kitReply(t, key, req, 10, 1, 3, "part one"))
	err := <-errs
	<-done
	if err == nil || !contains(err.Error(), "1 of 3 segments") {
		t.Fatalf("want a partial-reply timeout, got %v", err)
	}
}

// The announcing replica must not deliver its own announcement a second
// time, or a two-segment reply would be assembled from duplicates.
func TestAReplicaIgnoresItsOwnAnnouncement(t *testing.T) {
	a, tr, key := newService(t)
	shared := newFakeBus()
	_ = a.SetBus(shared)
	req, done, errs := pairAndSend(t, a, tr, key, "mgmt_status")
	ctx := context.Background()
	a.HandleInbound(ctx, BearerSMS, "+31653618463", kitReply(t, key, req, 10, 1, 2, "one"))
	select {
	case err := <-errs:
		t.Fatalf("a single segment echoed back through the bus completed the reply: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	a.HandleInbound(ctx, BearerSMS, "+31653618463", kitReply(t, key, req, 11, 2, 2, " two"))
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Body != "one two" {
		t.Fatalf("body %q", r.Body)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
