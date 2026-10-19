package hubmsg

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// fakeBus records what the service publishes; fail makes every publish fail.
type fakeBus struct {
	mu   sync.Mutex
	pubs []pub
	fail bool
}

type pub struct {
	topic   string
	payload []byte
}

func (b *fakeBus) Publish(topic string, qos byte, _ bool, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return errors.New("broker away")
	}
	if qos != 1 {
		return errors.New("hub_msg publishes QoS 1 only")
	}
	b.pubs = append(b.pubs, pub{topic, append([]byte(nil), payload...)})
	return nil
}

// take returns and forgets what was published on topic.
func (b *fakeBus) take(topic string) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	var got [][]byte
	var keep []pub
	for _, p := range b.pubs {
		if p.topic == topic {
			got = append(got, p.payload)
		} else {
			keep = append(keep, p)
		}
	}
	b.pubs = keep
	return got
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, b := range []struct{ tenant, id string }{{"t-1", "kit-a"}, {"t-1", "kit-b"}, {"t-2", "kit-z"}, {store.DefaultTenantID, "kit-p"}, {store.DefaultTenantID, "kit-q"}} {
		if err := db.CreateOrUpdateBridge(context.Background(), b.tenant, &store.Bridge{BridgeID: b.id, TenantID: b.tenant}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newService(st store.Store, b *fakeBus, c *clock) *Service {
	s := NewService(st, b)
	s.now = c.now
	return s
}

// submission builds a valid one from kit-a's point of view: mid and op id v7, digest right.
func submission(t *testing.T, at time.Time, to, body string) Submission {
	t.Helper()
	s := Submission{V: 1, OpID: NewV7(at), Mid: NewV7(at), To: to, ContentType: ContentTypeText,
		MidExpiresAt: at.Add(2 * time.Hour).UnixMilli(), ExpiresAt: at.Add(time.Hour).UnixMilli(), Body: body}
	d, err := ContentSHA256(s.Mid, s.To, s.ContentType, s.MidExpiresAt, []byte(s.Body))
	if err != nil {
		t.Fatal(err)
	}
	s.ContentSHA256 = d
	return s
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func events(t *testing.T, raw [][]byte) []Event {
	t.Helper()
	var out []Event
	for _, r := range raw {
		var e Event
		if err := json.Unmarshal(r, &e); err != nil {
			t.Fatal(err)
		}
		if e.EventID == "" {
			t.Fatalf("not an event: %s", r)
		}
		out = append(out, e)
	}
	return out
}

func classes(evs []Event) string {
	var c []string
	for _, e := range evs {
		c = append(c, e.Class)
	}
	return strings.Join(c, ",")
}

const (
	outA    = "meshsat/t-1/bridge/kit-a/msg/out"
	statusA = "meshsat/t-1/bridge/kit-a/msg/status"
	inB     = "meshsat/t-1/bridge/kit-b/msg/in"
	ackB    = "meshsat/t-1/bridge/kit-b/msg/ack"
)

// The route end to end on the Hub: kit-a submits, the outbox delivers to kit-b, kit-b stores
// and answers, kit-a gets forwarded then completed, the outbox stops.
func TestAnOperationIsDeliveredAnsweredAndReported(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "Water at the north gate")
	s.HandleOut(outA, mustJSON(t, sub))
	if got := b.take(statusA); len(got) != 0 {
		t.Fatalf("an admitted op got a status before anything happened: %s", got)
	}
	s.Drain(context.Background())
	in := b.take(inB)
	if len(in) != 1 {
		t.Fatalf("deliveries to kit-b: %d", len(in))
	}
	var d Delivery
	_ = json.Unmarshal(in[0], &d)
	if d.From != "bridge:kit-a" || d.To != "bridge:kit-b" || d.Body != sub.Body || d.ContentSHA256 != sub.ContentSHA256 || d.OpID != sub.OpID {
		t.Fatalf("delivery: %+v", d)
	}
	if got := events(t, b.take(statusA)); classes(got) != "forwarded" || got[0].Mid != sub.Mid || got[0].Rev != 1 {
		t.Fatalf("kit-a's evidence after the delivery: %+v", got)
	}

	s.HandleAck(ackB, mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"}))
	s.Drain(context.Background())
	got := events(t, b.take(statusA))
	if classes(got) != "completed" || got[0].Authority != "recipient" || got[0].Kind != "recipient_app_stored" || got[0].Rev != 2 {
		t.Fatalf("kit-a's evidence after kit-b stored it: %+v", got)
	}
	op, _ := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if op.State != "completed" || op.RecipientAnswer != "stored" {
		t.Fatalf("the operation: %+v", op)
	}
	c.add(time.Hour - time.Second)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("an answered op was delivered %d more times", n)
	}
}

// A repeat replays every event; a repeat with other fields is refused and changes nothing.
func TestARepeatReplaysEveryEventAndAConflictIsRefused(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "hello")
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	s.HandleAck(ackB, mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"}))
	s.Drain(context.Background())
	b.take(statusA)

	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	if got := events(t, b.take(statusA)); classes(got) != "forwarded,completed" {
		t.Fatalf("a repeat replayed %q, want every event", classes(got))
	}
	other := sub
	other.ExpiresAt--
	other.ContentSHA256 = sub.ContentSHA256 // same mid and digest, another deadline
	s.HandleOut(outA, mustJSON(t, other))
	raw := b.take(statusA)
	var r Refusal
	if len(raw) != 1 || json.Unmarshal(raw[0], &r) != nil || r.Type != "refusal" || r.Reason != "op_conflict" || r.OpID != sub.OpID {
		t.Fatalf("a conflicting repeat: %s", raw)
	}
	op, _ := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if op.State != "completed" || op.ExpiresAt != sub.ExpiresAt {
		t.Fatalf("a refused repeat changed the op: %+v", op)
	}
}

// The recipient must be a bridge of the sender's tenant; an answer is taken only from the
// recipient, naming the origin and the mid.
func TestTenantsRecipientsAndForgedAnswers(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	cross := submission(t, c.t, "bridge:kit-z", "across tenants")
	s.HandleOut(outA, mustJSON(t, cross))
	var r Refusal
	if raw := b.take(statusA); len(raw) != 1 || json.Unmarshal(raw[0], &r) != nil || r.Reason != "refused_unknown_recipient" {
		t.Fatalf("a recipient in another tenant: %s", raw)
	}

	sub := submission(t, c.t, "bridge:kit-b", "hello")
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	b.take(statusA)
	stored := Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"}
	s.HandleAck("meshsat/t-1/bridge/kit-a/msg/ack", mustJSON(t, stored)) // the sender answering its own op
	s.HandleAck("meshsat/t-2/bridge/kit-b/msg/ack", mustJSON(t, stored)) // the right id in another tenant
	wrongMid := stored
	wrongMid.Mid = NewV7(c.t)
	s.HandleAck(ackB, mustJSON(t, wrongMid))
	wrongFrom := stored
	wrongFrom.From = "bridge:kit-x"
	s.HandleAck(ackB, mustJSON(t, wrongFrom))
	s.Drain(context.Background())
	if got := b.take(statusA); len(got) != 0 {
		t.Fatalf("a forged answer made evidence: %s", got)
	}
	op, _ := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if op.State != "forwarded" || op.RecipientAnswer != "" {
		t.Fatalf("a forged answer changed the op: %+v", op)
	}

	// The platform tenant's shape works the same.
	plat := submission(t, c.t, "bridge:kit-q", "platform")
	s.HandleOut("meshsat/bridge/kit-p/msg/out", mustJSON(t, plat))
	s.Drain(context.Background())
	if n := len(b.take("meshsat/bridge/kit-q/msg/in")); n != 1 {
		t.Fatalf("platform-tenant deliveries: %d", n)
	}
}

// Without an answer the outbox publishes again on its backoff until the deadline, then records
// hub_expired; a busy recipient is retried; a late answer still completes the op.
func TestRetriesBusyExpiryAndALateAnswer(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "are you there")
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 1 {
		t.Fatalf("first delivery: %d", n)
	}
	c.add(10 * time.Second)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("delivered again before its backoff: %d", n)
	}
	c.add(10 * time.Second)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 1 {
		t.Fatalf("not delivered again after 15 s: %d", n)
	}
	s.HandleAck(ackB, mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "refused", Reason: "receiver_busy"}))
	c.add(5 * time.Minute)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 1 {
		t.Fatalf("a busy recipient is not tried again: %d", n)
	}
	got := events(t, b.take(statusA))
	if classes(got) != "forwarded,unknown" || got[1].Kind != "recipient_busy" {
		t.Fatalf("evidence: %q", classes(got))
	}
	c.add(time.Hour)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("delivered after its deadline: %d", n)
	}
	got = events(t, b.take(statusA))
	if classes(got) != "expired" || got[0].Kind != "hub_expired" || got[0].Reason != "expired" {
		t.Fatalf("after the deadline: %q", classes(got))
	}
	// A delivery the broker had queued is stored late: completed wins over expired.
	s.HandleAck(ackB, mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"}))
	op, _ := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if op.State != "completed" {
		t.Fatalf("a late answer: %s", op.State)
	}
}

// Two replicas get every message: one op, one event per evidence. A restarted Hub carries on
// from the store, and an event the broker did not take is published at the next pass.
func TestTwoReplicasARestartAndAFailedPublish(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	one, two := newService(st, b, c), newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "twice")
	one.HandleOut(outA, mustJSON(t, sub))
	two.HandleOut(outA, mustJSON(t, sub))
	one.Drain(context.Background())
	b.take(inB)
	if got := events(t, b.take(statusA)); classes(got) != "forwarded" {
		t.Fatalf("after the delivery: %q", classes(got))
	}
	ack := mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"})
	one.HandleAck(ackB, ack)
	two.HandleAck(ackB, ack)
	evs, _ := st.ListMsgOpEvents(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if len(evs) != 2 {
		t.Fatalf("two replicas recorded %d events, want forwarded and completed once each", len(evs))
	}

	b.fail = true
	restarted := newService(st, b, c)
	restarted.Drain(context.Background())
	b.fail = false
	if got := b.take(statusA); len(got) != 0 {
		t.Fatalf("a failed publish was recorded as published: %s", got)
	}
	restarted.Drain(context.Background())
	if got := events(t, b.take(statusA)); classes(got) != "completed" {
		t.Fatalf("after the broker came back: %q", classes(got))
	}
}

// What C4 refuses before the store, on the wire.
func TestAMalformedSubmissionIsRefusedWithAReason(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	ok := submission(t, c.t, "bridge:kit-b", "x")
	for _, tc := range []struct {
		name   string
		mutate func(*Submission)
		reason string
	}{
		{"v4 op id", func(s *Submission) { s.OpID = "0f8fad5b-d9cb-469f-a165-70867728950e" }, "bad_id"},
		{"a digest of other content", func(s *Submission) { s.Body = "y" }, "digest_mismatch"},
		{"a negative message deadline", func(s *Submission) { s.MidExpiresAt, s.ExpiresAt = -1, -2 }, "digest_mismatch"},
		{"deadline past the message's", func(s *Submission) { s.ExpiresAt = s.MidExpiresAt + 1 }, "bad_expiry"},
		{"a device recipient", func(s *Submission) { s.To = "device:phone-1"; s.ContentSHA256 = digestOf(t, *s) }, "bad_recipient"},
		{"another content type", func(s *Submission) { s.ContentType = "image/png"; s.ContentSHA256 = digestOf(t, *s) }, "bad_content_type"},
		{"version 2", func(s *Submission) { s.V = 2 }, "bad_version"},
		{"too large", func(s *Submission) { s.Body = strings.Repeat("a", MaxBody+1); s.ContentSHA256 = digestOf(t, *s) }, "too_large"},
		{"already past its deadline", func(s *Submission) {
			s.ExpiresAt = c.t.Add(-time.Second).UnixMilli()
		}, "expired_at_hub"},
	} {
		sub := ok
		sub.OpID, sub.Mid = NewV7(c.t), NewV7(c.t)
		sub.ContentSHA256 = digestOf(t, sub)
		tc.mutate(&sub)
		s.HandleOut(outA, mustJSON(t, sub))
		var r Refusal
		raw := b.take(statusA)
		if len(raw) != 1 || json.Unmarshal(raw[0], &r) != nil || r.Reason != tc.reason {
			t.Errorf("%s: %s, want %s", tc.name, raw, tc.reason)
		}
	}
	s.HandleOut(outA, []byte("{not json"))
	if raw := b.take(statusA); len(raw) != 1 || !strings.Contains(string(raw[0]), "bad_json") {
		t.Fatalf("not JSON: %s", raw)
	}
}

func digestOf(t *testing.T, s Submission) string {
	t.Helper()
	d, err := ContentSHA256(s.Mid, s.To, s.ContentType, s.MidExpiresAt, []byte(s.Body))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A recipient taken out of the tenant after the operation was admitted: the outbox publishes
// nothing for it, records hub_refused (refused_unknown_recipient) and stops delivering.
func TestARecipientRemovedAfterAdmissionIsRefusedByTheHub(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "after the removal")
	s.HandleOut(outA, mustJSON(t, sub))
	if err := st.DeleteBridge(context.Background(), "t-1", "kit-b"); err != nil {
		t.Fatal(err)
	}
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("published to a removed recipient: %d", n)
	}
	got := events(t, b.take(statusA))
	if len(got) != 1 || got[0].Kind != "hub_refused" || got[0].Class != "failed" || got[0].Reason != "refused_unknown_recipient" {
		t.Fatalf("evidence: %+v", got)
	}
	op, _ := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if op.State != "failed" {
		t.Fatalf("state %s", op.State)
	}
	c.add(2 * time.Hour)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("a final operation was delivered again: %d", n)
	}
}

// The clock that decides is the service's: an operation admitted by a replica whose clock is
// behind the database host's is due at once, not when the store's own time comes round (the
// flake that found it: the first drain missed a submission made a moment earlier).
func TestTheServiceClockDecidesWhenAnOperationIsDue(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC().Add(-10 * time.Second)}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "behind the database clock")
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 1 {
		t.Fatalf("deliveries on the first drain: %d", n)
	}
	// The lease is the service's too: the next drain, before the backoff, publishes nothing.
	c.add(time.Second)
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 0 {
		t.Fatalf("delivered again inside the backoff: %d", n)
	}
	c.add(Backoff(1))
	s.Drain(context.Background())
	if n := len(b.take(inB)); n != 1 {
		t.Fatalf("not delivered again after the backoff: %d", n)
	}
}
