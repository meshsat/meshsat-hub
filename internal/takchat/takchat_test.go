package takchat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// MESHSAT-1706. The victim of every isolation test is store.DefaultTenantID.

type sent struct {
	topic   string
	qos     byte
	payload string
}

type fakeBus struct {
	mu   sync.Mutex
	sent []sent
	subs map[string]bus.MessageHandler
	fail error
}

func (b *fakeBus) Publish(topic string, qos byte, _ bool, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.sent = append(b.sent, sent{topic, qos, string(payload)})
	return nil
}

func (b *fakeBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[string]bus.MessageHandler{}
	}
	b.subs[topic] = h
	return nil
}

func (b *fakeBus) published() []sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sent(nil), b.sent...)
}

func rig(t *testing.T) (*Service, *fakeBus, store.Store, time.Time) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for tenant, bridges := range map[string][]string{"t1": {"kit-a", "kit-b"}, store.DefaultTenantID: {"kit-v"}} {
		_ = db.CreateTenant(ctx, &store.Tenant{ID: tenant, Slug: tenant, Name: tenant, Status: store.TenantActive})
		for _, id := range bridges {
			if err := db.CreateOrUpdateBridge(ctx, tenant, &store.Bridge{BridgeID: id, Label: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []store.TAKContact{
		{TenantID: "t1", UID: "meshsat-a1b3c3a4", Callsign: "MESHSAT-c3a4", BridgeID: "kit-a", LastSeen: now},
		{TenantID: "t1", UID: "meshsat-11112222", Callsign: "MESHSAT-2222", BridgeID: "kit-b", LastSeen: now},
		{TenantID: "t1", UID: "meshsat-0ld0ld00", Callsign: "OLD", BridgeID: "kit-a", LastSeen: now.Add(-25 * time.Hour)},
		{TenantID: "t1", UID: "meshsat-90000000", Callsign: "GONE", BridgeID: "kit-gone", LastSeen: now},
		{TenantID: store.DefaultTenantID, UID: "meshsat-77777777", Callsign: "VICTIM", BridgeID: "kit-v", LastSeen: now},
	} {
		if err := db.UpsertTAKContact(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	b := &fakeBus{}
	s := New(db, b, true, nil)
	s.now = func() time.Time { return now }
	return s, b, db, now
}

func TestAnOperatorsMessageGoesToTheOneClientThatOwnsTheCrew(t *testing.T) {
	s, b, db, _ := rig(t)
	msg, created, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4",
		UserID: "7", Name: "Anna", Text: "check in\nplease"})
	if err != nil || !created {
		t.Fatalf("send: %v %v", created, err)
	}
	got := b.published()
	if len(got) != 1 || got[0].topic != hubmqtt.TopicTAKCotInFor("t1", "kit-a") || got[0].qos != 1 {
		t.Fatalf("published %+v", got)
	}
	_, meta, err := tak.Sanitize([]byte(got[0].payload), tak.ClientLimits, time.Now())
	if err != nil {
		t.Fatalf("the Hub's own chat fails its sanitiser: %v", err)
	}
	if uid, cs := meta.Sender(); uid != tak.OperatorUIDPrefix+"7" || cs != "Anna" {
		t.Errorf("from %q %q", uid, cs)
	}
	if to := meta.Addressees(); len(to) != 1 || to[0] != (tak.Dest{UID: "meshsat-a1b3c3a4", Callsign: "MESHSAT-c3a4"}) {
		t.Errorf("to %v", to)
	}
	if meta.Remarks != "check in please" {
		t.Errorf("text %q: a radio shows one line", meta.Remarks)
	}
	rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-a1b3c3a4", 10)
	if len(rows) != 1 || rows[0].ID != msg.ID || rows[0].Direction != "out" || rows[0].Status != "sent" || rows[0].BridgeID != "kit-a" {
		t.Errorf("stored %+v", rows)
	}
}

func TestASendIsRefusedForEachReason(t *testing.T) {
	long := strings.Repeat("x", MaxBytes+1)
	cases := []struct {
		name string
		req  SendRequest
		want error
	}{
		{"no text", SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: " \n "}, ErrEmpty},
		{"too long", SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: long}, ErrTooLong},
		{"no such crew", SendRequest{TenantID: "t1", CrewUID: "meshsat-deadbeef", Text: "hi"}, ErrNoCrew},
		{"another tenant's crew", SendRequest{TenantID: "t1", CrewUID: "meshsat-77777777", Text: "hi"}, ErrNoCrew},
		{"not seen for a day", SendRequest{TenantID: "t1", CrewUID: "meshsat-0ld0ld00", Text: "hi"}, ErrStale},
		{"its client is gone", SendRequest{TenantID: "t1", CrewUID: "meshsat-90000000", Text: "hi"}, ErrUnregistered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, b, _, _ := rig(t)
			if _, _, err := s.Send(context.Background(), c.req); !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			if len(b.published()) != 0 {
				t.Error("a refused send was published")
			}
		})
	}
	s, b, _, _ := rig(t)
	s.enabled = false
	if _, _, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: "hi"}); !errors.Is(err, ErrDisabled) || len(b.published()) != 0 {
		t.Errorf("off: %v", err)
	}
}

func TestAResentRequestIsOneMessage(t *testing.T) {
	s, b, db, _ := rig(t)
	req := SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", UserID: "7", Text: "hi", ClientRef: "r-1"}
	first, created, err := s.Send(context.Background(), req)
	if err != nil || !created {
		t.Fatal(err)
	}
	s.now = func() time.Time { return first.CreatedAt.Add(crewWindow) } // past the per-crew window
	again, created, err := s.Send(context.Background(), req)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("the repeat: %+v %v %v", again, created, err)
	}
	if n := len(b.published()); n != 1 {
		t.Errorf("published %d times", n)
	}
	if rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-a1b3c3a4", 10); len(rows) != 1 {
		t.Errorf("%d rows", len(rows))
	}
}

func TestOneMessagePerCrewPerWindow(t *testing.T) {
	s, b, _, _ := rig(t)
	ctx := context.Background()
	if _, _, err := s.Send(ctx, SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Send(ctx, SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: "two"}); !errors.Is(err, ErrLimited) {
		t.Errorf("a second message inside the window: %v", err)
	}
	// Positive control: another crew is its own window.
	if _, _, err := s.Send(ctx, SendRequest{TenantID: "t1", CrewUID: "meshsat-11112222", Text: "other"}); err != nil {
		t.Errorf("another crew was limited: %v", err)
	}
	if n := len(b.published()); n != 2 {
		t.Errorf("published %d", n)
	}
}

func TestAFailedPublishIsRecordedAsFailed(t *testing.T) {
	s, b, db, _ := rig(t)
	b.fail = errors.New("bus down")
	if _, _, err := s.Send(context.Background(), SendRequest{TenantID: "t1", CrewUID: "meshsat-a1b3c3a4", Text: "hi"}); !errors.Is(err, ErrPublish) {
		t.Fatalf("err %v", err)
	}
	rows, _ := db.ListTAKChatMessages(context.Background(), "t1", "meshsat-a1b3c3a4", 10)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Errorf("stored %+v", rows)
	}
}

func TestOperatorPeerNeverCarriesAnEmail(t *testing.T) {
	for _, c := range []struct{ id, name, uid, callsign string }{
		{"7", "Anna", tak.OperatorUIDPrefix + "7", "Anna"},
		{"7", "anna@example.org", tak.OperatorUIDPrefix + "7", "Office"},
		{"", "", tak.OperatorUIDPrefix + "api", "Office"},
		{"a b/c", strings.Repeat("N", 40), tak.OperatorUIDPrefix + "abc", strings.Repeat("N", maxCallsign)},
	} {
		if got := OperatorPeer(c.id, c.name); got.UID != c.uid || got.Callsign != c.callsign {
			t.Errorf("OperatorPeer(%q, %q) = %+v", c.id, c.name, got)
		}
	}
}

// answer is a crew's GeoChat to an operator, as the Bridge builds it.
func answer(t *testing.T, fromUID, fromCS, toUID, text, id string) []byte {
	t.Helper()
	xml, err := tak.MarshalCotEvent(tak.BuildGeoChat(tak.ChatPeer{UID: fromUID, Callsign: fromCS},
		tak.ChatPeer{UID: toUID, Callsign: "Anna"}, text, id, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return xml
}

func TestACrewsAnswerIsStoredOnceFromItsOwnClientOnly(t *testing.T) {
	s, _, db, _ := rig(t)
	ctx := context.Background()
	op := tak.OperatorUIDPrefix + "7"
	ok := answer(t, "meshsat-a1b3c3a4", "MESHSAT-c3a4", op, "on my way", "ans-1")
	// Both replicas receive it.
	s.store1(ctx, reply{hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), ok})
	s.store1(ctx, reply{hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), ok})
	// kit-b answering in kit-a's crew's name; an answer to nobody; an unregistered sender.
	s.store1(ctx, reply{hubmqtt.TopicTAKCotOutFor("t1", "kit-b"), answer(t, "meshsat-a1b3c3a4", "MESHSAT-c3a4", op, "forged", "ans-2")})
	s.store1(ctx, reply{hubmqtt.TopicTAKCotOutFor("t1", "kit-a"), answer(t, "meshsat-a1b3c3a4", "MESHSAT-c3a4", "ANDROID-abc", "to a TAK user", "ans-3")})
	s.store1(ctx, reply{hubmqtt.TopicTAKCotOutFor("t1", "kit-x"), answer(t, "meshsat-a1b3c3a4", "MESHSAT-c3a4", op, "stranger", "ans-4")})
	rows, _ := db.ListTAKChatMessages(ctx, "t1", "meshsat-a1b3c3a4", 10)
	if len(rows) != 1 || rows[0].Text != "on my way" || rows[0].Direction != "in" || rows[0].UserID != "7" || rows[0].ID != "ans-1" {
		t.Fatalf("stored %+v", rows)
	}
	// The victim tenant's identical crew uid got nothing.
	if v, _ := db.ListTAKChatMessages(ctx, store.DefaultTenantID, "meshsat-a1b3c3a4", 10); len(v) != 0 {
		t.Errorf("the default tenant got %+v", v)
	}
}

func TestTheSinkHandsOverOnlyWhatMayBeAnAnswer(t *testing.T) {
	s, b, _, _ := rig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.StartReplySink(ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.subs) != len(hubmqtt.TAKCotOutFilters()) {
		t.Fatalf("subscribed %d filters", len(b.subs))
	}
	s.receive("x", []byte(`<event uid="meshsat-a1b3c3a4" type="a-f-G-U-C"/>`))
	if len(s.replies) != 0 {
		t.Error("a position was queued as a possible answer")
	}
	s.receive("x", answer(t, "meshsat-a1b3c3a4", "MESHSAT-c3a4", tak.OperatorUIDPrefix+"7", "hi", "q"))
	deadline := time.Now().Add(time.Second)
	for len(s.replies) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}
