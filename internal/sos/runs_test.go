package sos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/hubmsg"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// capBus records every publish with its payload.
type capBus struct {
	mu   sync.Mutex
	pubs []capPub
	hold chan struct{} // set: each publish waits for it to close, as a real one waits for its PUBACK
}

type capPub struct {
	topic    string
	retained bool
	body     map[string]any
}

func (b *capBus) Connect() error                                                { return nil }
func (b *capBus) Disconnect()                                                   {}
func (b *capBus) IsConnected() bool                                             { return true }
func (b *capBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *capBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (b *capBus) Publish(topic string, _ byte, retained bool, p []byte) error {
	if b.hold != nil {
		<-b.hold
	}
	var m map[string]any
	_ = json.Unmarshal(p, &m)
	b.mu.Lock()
	b.pubs = append(b.pubs, capPub{topic, retained, m})
	b.mu.Unlock()
	return nil
}
func (b *capBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	return b.Publish(topic, qos, retained, data)
}

func (b *capBus) on(suffix string) []capPub {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []capPub
	for _, p := range b.pubs {
		if strings.HasSuffix(p.topic, suffix) {
			out = append(out, p)
		}
	}
	return out
}

const tenant = "default"

type harness struct {
	st     store.Store
	bus    *capBus
	engine *escalation.Engine
	runs   *Runs
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := newDetectorStore(t)
	ctx := context.Background()
	if err := st.CreateEscalationChain(ctx, tenant, &store.EscalationChain{ID: "chain-1", Name: "on call",
		Tiers: []store.EscalationTier{{Name: "sms", Targets: []string{"x"}, WaitSec: 60}}}); err != nil {
		t.Fatal(err)
	}
	b := &capBus{}
	engine := escalation.New(st, nil)
	return &harness{st: st, bus: b, engine: engine,
		runs: NewRuns(st, b, engine, func(string) string { return "chain-1" }, nil)}
}

// pages counts the alerts that paged. A cancel that came before its start writes the run's
// alert closed (a tombstone, C10-R6), which paged nobody.
func pages(alerts []store.Alert) int {
	n := 0
	for _, a := range alerts {
		if !strings.HasSuffix(a.Detail, escalation.ClosedBeforeRaised) {
			n++
		}
	}
	return n
}

func (h *harness) alerts(t *testing.T) []store.Alert {
	t.Helper()
	a, err := h.st.ListAlerts(context.Background(), tenant, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fixture holds the parts of docs/fixtures/v1/sos.json the service is checked against.
type fixture struct {
	Legacy struct {
		BridgeID   string `json:"bridge_id"`
		RunStartMs int64  `json:"run_start_ms"`
		SOSID      string `json:"sos_id"`
	} `json:"legacy_sos_id"`
	MQTT []struct {
		Case  string         `json:"case"`
		JSON  map[string]any `json:"json"`
		Valid bool           `json:"valid"`
	} `json:"mqtt"`
	States []struct {
		Case  string `json:"case"`
		Steps []struct {
			Event  string `json:"event"`
			SOSID  string `json:"sos_id"`
			Bearer string `json:"bearer"`
		} `json:"steps"`
		Expect struct {
			State string `json:"state"`
			Pages int    `json:"pages"`
		} `json:"expect"`
	} `json:"states"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("../../docs/fixtures/v1/sos.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// runStart is the start a fixture id names: the legacy run's, or a v7 id's own time.
func (f fixture) runStart(t *testing.T, id string) int64 {
	if id == f.Legacy.SOSID {
		return f.Legacy.RunStartMs
	}
	b, ok := hubmsg.UUIDBytes(id)
	if !ok {
		t.Fatalf("bad id %s", id)
	}
	return hubmsg.TimeOfV7(b)
}

// Every state case of the shared fixture, through the service: one page per run whatever
// bearers carry its start, a cancel is terminal even before the start, and a new run pages
// again. Each cancelled run's alert is closed.
func TestRunsFollowTheFixtureStates(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.States {
		t.Run(c.Case, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			for _, st := range c.Steps {
				bearer := st.Bearer
				if bearer == "" {
					bearer = "mqtt"
				}
				if _, err := h.runs.Apply(ctx, Event{TenantID: tenant, BridgeID: f.Legacy.BridgeID, Bearer: bearer, Kind: st.Event,
					SOSID: st.SOSID, RunStartMs: f.runStart(t, st.SOSID), Text: "Injured, need help"}); err != nil {
					t.Fatal(err)
				}
			}
			last := c.Steps[len(c.Steps)-1].SOSID
			run, err := h.st.GetSOSRun(ctx, tenant, Origin(f.Legacy.BridgeID), last)
			if err != nil || run.State != c.Expect.State {
				t.Fatalf("state %+v %v, want %s", run, err, c.Expect.State)
			}
			alerts := h.alerts(t)
			if got := pages(alerts); got != c.Expect.Pages {
				t.Fatalf("%d of %d alerts paged, want %d pages", got, len(alerts), c.Expect.Pages)
			}
			runs, _ := h.st.ListSOSRuns(ctx, tenant, 10)
			for _, r := range runs {
				a, err := h.st.GetAlert(ctx, tenant, AlertID(tenant, r.Origin, r.SOSID))
				if err != nil {
					continue // a run cancelled before its start never paged
				}
				want := store.AlertStateTriggered
				if r.State == store.SOSRunCancelled {
					want = store.AlertStateCancelled
				}
				if a.State != want {
					t.Errorf("run %s (%s): alert %s, want %s", r.SOSID, r.State, a.State, want)
				}
			}
		})
	}
}

// Every start and cancel from the bridge is acknowledged on its sos/ack with the run's state
// and revision; a start after the cancel is answered "cancelled", so the bridge stops.
func TestEveryEventIsAcknowledged(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	ev := Event{TenantID: tenant, BridgeID: "kit-a", Bearer: "imt", Kind: store.SOSEventStart, SOSID: f.Legacy.SOSID,
		RunStartMs: f.Legacy.RunStartMs}
	for _, kind := range []string{store.SOSEventStart, store.SOSEventCancel, store.SOSEventStart} {
		ev.Kind = kind
		if _, err := h.runs.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	acks := h.bus.on("/sos/ack")
	if len(acks) != 3 {
		t.Fatalf("%d acks, want 3", len(acks))
	}
	want := []struct {
		kind, state string
		rev         float64
	}{{"start", "active", 1}, {"cancel", "cancelled", 2}, {"start", "cancelled", 3}}
	for i, w := range want {
		a := acks[i]
		if a.topic != hubmqtt.TopicSOSAckFor(tenant, "kit-a") || a.retained || a.body["kind"] != w.kind ||
			a.body["state"] != w.state || a.body["rev"] != w.rev || a.body["sos_id"] != f.Legacy.SOSID || a.body["v"] != 2.0 {
			t.Errorf("ack %d: %s %+v, want %+v", i, a.topic, a.body, w)
		}
	}
	// The Hub's consumers heard the start and the end, once each.
	if got := h.bus.on("/sos"); len(got) != 2 || got[0].body["state"] != "active" || got[1].body["state"] != "cancelled" {
		t.Errorf("SOS topic: %+v", got)
	}
}

// An operator's cancel closes the run and its alert, says who, and sends the bridge nothing;
// the bridge's next start pages nobody.
func TestAnOperatorCancelIsTerminalAndLogged(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	start := Event{TenantID: tenant, BridgeID: "kit-a", Bearer: "imt", Kind: store.SOSEventStart, SOSID: f.Legacy.SOSID,
		RunStartMs: f.Legacy.RunStartMs}
	if _, err := h.runs.Apply(ctx, start); err != nil {
		t.Fatal(err)
	}
	runs, err := h.runs.Cancel(ctx, tenant, f.Legacy.SOSID, "u1")
	if err != nil || len(runs) != 1 || runs[0].State != store.SOSRunCancelled || runs[0].CancelledBy != "user:u1" {
		t.Fatalf("cancel: %+v %v", runs, err)
	}
	a, err := h.st.GetAlert(ctx, tenant, AlertID(tenant, Origin("kit-a"), f.Legacy.SOSID))
	if err != nil || a.State != store.AlertStateCancelled || a.AckedBy != "user:u1" {
		t.Fatalf("alert: %+v %v", a, err)
	}
	if acks := h.bus.on("/sos/ack"); len(acks) != 1 {
		t.Errorf("%d acks, want only the start's", len(acks))
	}
	if _, err := h.runs.Apply(ctx, start); err != nil {
		t.Fatal(err)
	}
	if n := len(h.alerts(t)); n != 1 {
		t.Errorf("%d alerts after a start following the cancel, want 1", n)
	}
	if _, err := h.runs.Cancel(ctx, tenant, "0199b7c4-f53b-7000-8000-000000000009", "u1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown run: %v", err)
	}
}

// Two replicas see the same start: one alert. The bus hands both every message.
func TestTwoReplicasPageOnce(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	other := NewRuns(h.st, h.bus, h.engine, func(string) string { return "chain-1" }, nil)
	ctx := context.Background()
	ev := Event{TenantID: tenant, BridgeID: "kit-a", Bearer: "mqtt", Kind: store.SOSEventStart, SOSID: f.Legacy.SOSID,
		RunStartMs: f.Legacy.RunStartMs}
	var wg sync.WaitGroup
	for _, r := range []*Runs{h.runs, other, h.runs, other} {
		wg.Add(1)
		go func(r *Runs) {
			defer wg.Done()
			if _, err := r.Apply(ctx, ev); err != nil {
				t.Error(err)
			}
		}(r)
	}
	wg.Wait()
	if n := len(h.alerts(t)); n != 1 {
		t.Errorf("%d alerts, want 1", n)
	}
}

// cancelOnAlert is a store whose SetSOSRunAlert first lets another replica cancel the run:
// the cancel is applied after the start but before the alert is recorded, so that replica
// found no alert to close.
type cancelOnAlert struct {
	store.Store
	once   sync.Once
	cancel *store.SOSRunEvent
}

func (c *cancelOnAlert) SetSOSRunAlert(ctx context.Context, tenantID, origin, sosID, alertID string) error {
	c.once.Do(func() { _, _ = c.ApplySOSRunEvent(ctx, c.cancel) })
	return c.Store.SetSOSRunAlert(ctx, tenantID, origin, sosID, alertID)
}

// A cancel that lands between the start and its alert still closes the alert.
func TestACancelDuringTheStartsAlertClosesIt(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	wrapped := &cancelOnAlert{Store: h.st, cancel: &store.SOSRunEvent{TenantID: tenant, Origin: Origin("kit-a"),
		SOSID: f.Legacy.SOSID, BridgeID: "kit-a", RunStartMs: f.Legacy.RunStartMs, Kind: store.SOSEventCancel, Bearer: "sms",
		By: Origin("kit-a")}}
	runs := NewRuns(wrapped, h.bus, h.engine, func(string) string { return "chain-1" }, nil)
	if _, err := runs.Apply(ctx, Event{TenantID: tenant, BridgeID: "kit-a", Bearer: "imt", Kind: store.SOSEventStart,
		SOSID: f.Legacy.SOSID, RunStartMs: f.Legacy.RunStartMs}); err != nil {
		t.Fatal(err)
	}
	a, err := h.st.GetAlert(ctx, tenant, AlertID(tenant, Origin("kit-a"), f.Legacy.SOSID))
	if err != nil || a.State != store.AlertStateCancelled {
		t.Fatalf("alert: %+v %v", a, err)
	}
}

// An id that does not name its run is refused, and nothing is stored or acknowledged.
func TestAnIDThatDoesNotNameItsRunIsRefused(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	for _, ev := range []Event{
		{Kind: store.SOSEventStart, BridgeID: "kit-a", SOSID: f.Legacy.SOSID, RunStartMs: f.Legacy.RunStartMs / 1000 * 1000},
		{Kind: store.SOSEventCancel, BridgeID: "kit-b", SOSID: f.Legacy.SOSID, RunStartMs: f.Legacy.RunStartMs},
		{Kind: store.SOSEventStart, BridgeID: "kit-a", SOSID: hubmsg.NewV7(timeOfMs(f.Legacy.RunStartMs + 1)), RunStartMs: f.Legacy.RunStartMs},
	} {
		ev.TenantID, ev.Bearer = tenant, "mqtt"
		if _, err := h.runs.Apply(ctx, ev); !errors.Is(err, ErrBadSOSID) {
			t.Errorf("%+v: %v", ev, err)
		}
	}
	if runs, _ := h.st.ListSOSRuns(ctx, tenant, 10); len(runs) != 0 || len(h.bus.pubs) != 0 {
		t.Errorf("stored %d runs, published %d", len(runs), len(h.bus.pubs))
	}
}

// The fixture's MQTT forms on the bridge's own sos/event: valid ones make the run, and a
// start that cannot be taken still pages the old way (mo/decoded, sos=true) and makes no run.
func TestMQTTEventsFollowTheFixture(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	topic := "meshsat/bridge/kit-a/sos/event"
	for _, c := range f.MQTT {
		before := len(h.bus.on("/mo/decoded"))
		payload, _ := json.Marshal(c.JSON)
		h.runs.handleMQTT(topic, payload)
		run, err := h.st.GetSOSRun(ctx, tenant, Origin("kit-a"), f.Legacy.SOSID)
		legacy := len(h.bus.on("/mo/decoded")) - before
		switch {
		case c.Valid && c.JSON["kind"] == "start":
			if err != nil || run.State != store.SOSRunActive || legacy != 0 {
				t.Errorf("%s: run %+v %v, old-way pages %d", c.Case, run, err, legacy)
			}
		case c.Valid:
			if err != nil || run.State != store.SOSRunCancelled {
				t.Errorf("%s: run %+v %v", c.Case, run, err)
			}
		default:
			if legacy != 1 {
				t.Errorf("%s: %d old-way pages, want 1", c.Case, legacy)
			}
		}
	}
	page := h.bus.on("/mo/decoded")
	if len(page) != 1 || page[0].body["sos"] != true || page[0].body["bridge_id"] != "kit-a" {
		t.Errorf("old-way page: %+v", page)
	}
	// A refused cancel changes nothing and pages nobody.
	before := len(h.bus.pubs)
	h.runs.handleMQTT(topic, []byte(`{"v":2,"kind":"cancel","sos_id":"`+f.Legacy.SOSID+`"}`))
	if len(h.bus.pubs) != before {
		t.Error("a refused cancel published something")
	}
	// The tenant shape names the tenant.
	if tn, id, ok := hubmqtt.ParseSOSEventTopic("meshsat/t_x/bridge/kit-a/sos/event"); !ok || tn != "t_x" || id != "kit-a" {
		t.Errorf("tenant topic: %s %s %v", tn, id, ok)
	}
}

func timeOfMs(ms int64) time.Time { return time.UnixMilli(ms) }

// A run already cancelled whose alert is still open (another replica raised it after the
// first copy of the cancel was applied) is closed by the next copy of the cancel.
func TestAnotherCopyOfTheCancelClosesAnAlertLeftOpen(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	start := Event{TenantID: tenant, BridgeID: "kit-a", Bearer: "imt", Kind: store.SOSEventStart, SOSID: f.Legacy.SOSID,
		RunStartMs: f.Legacy.RunStartMs}
	if _, err := h.runs.Apply(ctx, start); err != nil {
		t.Fatal(err)
	}
	// The first copy of the cancel reached the store only (its replica stopped there).
	if _, err := h.st.ApplySOSRunEvent(ctx, &store.SOSRunEvent{TenantID: tenant, Origin: Origin("kit-a"), SOSID: f.Legacy.SOSID,
		BridgeID: "kit-a", RunStartMs: f.Legacy.RunStartMs, Kind: store.SOSEventCancel, Bearer: "sms", By: Origin("kit-a")}); err != nil {
		t.Fatal(err)
	}
	cancel := start
	cancel.Kind, cancel.Bearer = store.SOSEventCancel, "mqtt"
	if _, err := h.runs.Apply(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	a, err := h.st.GetAlert(ctx, tenant, AlertID(tenant, Origin("kit-a"), f.Legacy.SOSID))
	if err != nil || a.State != store.AlertStateCancelled {
		t.Fatalf("alert %+v %v", a, err)
	}
}

// The sos/event handler never holds the bus's handler goroutine: it publishes (the ack), and a
// publish waits for a PUBACK the MQTT client cannot read while a handler holds its router.
func TestTheEventHandlerNeverHoldsTheBus(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	h.bus.hold = make(chan struct{})
	payload, _ := json.Marshal(f.MQTT[0].JSON)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			h.runs.HandleMQTT("meshsat/bridge/kit-a/sos/event", payload)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(h.bus.hold)
		t.Fatal("HandleMQTT waited for its publish")
	}
	close(h.bus.hold)
	deadline := time.Now().Add(5 * time.Second)
	for len(h.bus.on("/sos/ack")) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(h.bus.on("/sos/ack")); n != 3 {
		t.Fatalf("%d acks, want 3", n)
	}
}
