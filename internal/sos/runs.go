package sos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/hubmsg"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/dbwrap"
)

// Runs is the Hub's side of an SOS raised under its own identity (HeMB-NG stage 2, C10,
// MESHSAT-1584; docs/bridge-hub-protocol.md "SOS identity (C10)"). Every start and
// cancel names its run by sos_id and run_start_ms, whichever bearer carries it: the
// satellite frames 0x07/0x08, an SMS-borne frame, or v2 JSON on the bridge's sos/event.
//
//   - A run pages once, under the deterministic alert id AlertID, whichever bearers and
//     replicas see its start.
//   - A cancel is terminal: it stores the run cancelled even before the start arrives,
//     closes its alert, and a start that comes after it pages nobody.
//   - The bridge gets sos/ack for every start and cancel it sent, so it can stop retrying.
//     The ack follows the run's commit, and the alert work the run owes is in the same
//     row (alert_pending): what an apply could not finish, the reconciler does.
//
// The old frames (0x02, mo/decoded with sos=true) keep their own path (Detector).
type Runs struct {
	store   store.Store
	bus     bus.MessageBus
	engine  *escalation.Engine
	chainOf func(tenantID string) string
	audit   *audit.Service
	now     func() time.Time
}

// NewRuns wires the service. engine may be nil (alerts are then only logged), and chainOf
// names the tenant's escalation chain ("" for none), as for the Detector.
func NewRuns(st store.Store, b bus.MessageBus, engine *escalation.Engine, chainOf func(string) string, a *audit.Service) *Runs {
	return &Runs{store: st, bus: b, engine: engine, chainOf: chainOf, audit: a, now: time.Now}
}

// ErrBadSOSID is a start or cancel whose sos_id does not name its run (C10 rule).
var ErrBadSOSID = errors.New("sos: sos_id does not name this run")

// Event is one start or cancel as an ingress authenticated it.
type Event struct {
	TenantID string
	// BridgeID is the bridge the ingress authenticated: the topic over MQTT, the modem's
	// bridge over satellite, the tenant's bridge named in an SMS-borne frame.
	BridgeID string
	Bearer   string // "mqtt", "sbd", "imt", "sms", "hub" (an operator on the Hub)
	Kind     string // store.SOSEventStart or store.SOSEventCancel
	SOSID    string
	// RunStartMs is the run's start as the sender carries it; the sos_id must name it.
	RunStartMs int64
	// A start's content.
	DeviceID, Text string
	Lat, Lon       float64
	HasPosition    bool
	// A cancel's reason: 0 cancelled by the operator, 1 test ended.
	Reason int
	// By is who cancelled, when not the origin itself ("user:<id>" for an operator).
	By string
}

// Origin is the principal a bridge raises its SOS as.
func Origin(bridgeID string) string { return "bridge:" + bridgeID }

// AlertID is the escalation alert a run raises: one per run, whatever raises it, and
// never the same for two runs of different tenants or origins. It fits alerts.id (64).
func AlertID(tenantID, origin, sosID string) string {
	h := sha256.Sum256([]byte(tenantID + "\x00" + origin + "\x00" + sosID))
	return "sos-" + hex.EncodeToString(h[:20])
}

// Apply validates ev against the C10 rule and applies it: it records the run, raises or
// closes its alert, tells the Hub's consumers, and acknowledges the bridge.
func (r *Runs) Apply(ctx context.Context, ev Event) (store.SOSRunApplied, error) {
	if ev.Kind != store.SOSEventStart && ev.Kind != store.SOSEventCancel {
		return store.SOSRunApplied{}, fmt.Errorf("sos: unknown event kind %q", ev.Kind)
	}
	if ev.TenantID == "" || ev.BridgeID == "" || !hubmsg.SOSIDValid(ev.SOSID, ev.BridgeID, ev.RunStartMs) {
		return store.SOSRunApplied{}, ErrBadSOSID
	}
	return r.apply(ctx, ev, Origin(ev.BridgeID))
}

func (r *Runs) apply(ctx context.Context, ev Event, origin string) (store.SOSRunApplied, error) {
	by := ev.By
	if by == "" {
		by = origin
	}
	var applied store.SOSRunApplied
	err := dbwrap.RetryShort(ctx, func() error {
		var aerr error
		applied, aerr = r.store.ApplySOSRunEvent(ctx, &store.SOSRunEvent{
			TenantID: ev.TenantID, Origin: origin, SOSID: ev.SOSID, BridgeID: ev.BridgeID, RunStartMs: ev.RunStartMs,
			Kind: ev.Kind, Bearer: ev.Bearer, DeviceID: ev.DeviceID, Text: ev.Text, Lat: ev.Lat, Lon: ev.Lon,
			HasPosition: ev.HasPosition, By: by, Reason: ev.Reason, At: r.now().UTC(),
		})
		return aerr
	})
	if err != nil {
		return applied, fmt.Errorf("sos: apply %s: %w", ev.Kind, err)
	}
	run := applied.Run
	slog.Warn("sos: run "+ev.Kind, "tenant", ev.TenantID, "origin", origin, "sos_id", ev.SOSID, "bearer", ev.Bearer,
		"state", run.State, "rev", run.Rev, "created", applied.Created, "ended", applied.Ended)

	// The alert work this state change owes, committed with it: done now, or by the
	// reconciler when it fails here. The ack below stands for that durable obligation.
	if run.AlertPending {
		r.settle(ctx, run)
	}
	if applied.Created && run.State == store.SOSRunActive || applied.Ended {
		r.tell(run)
	}
	if ev.By == "" {
		r.ack(run, ev.Kind)
	}
	r.log(ctx, run, ev, by)
	return applied, nil
}

// Cancel is an operator's cancel on the Hub: every run of the tenant with this sos_id
// (normally one) is cancelled and its alert closed, logged with who did it. ErrNotFound
// when the tenant has no such run.
func (r *Runs) Cancel(ctx context.Context, tenantID, sosID, userID string) ([]store.SOSRun, error) {
	runs, err := r.store.FindSOSRuns(ctx, tenantID, sosID)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, store.ErrNotFound
	}
	out := make([]store.SOSRun, 0, len(runs))
	for _, run := range runs {
		applied, err := r.apply(ctx, Event{
			TenantID: tenantID, BridgeID: run.BridgeID, Bearer: "hub", Kind: store.SOSEventCancel, SOSID: run.SOSID,
			RunStartMs: run.RunStartMs, Reason: 0, By: "user:" + userID,
		}, run.Origin)
		if err != nil {
			return out, err
		}
		out = append(out, applied.Run)
	}
	return out, nil
}

// settle does the alert work run owes and, when it succeeded, clears the obligation for the
// run's rev; an event applied meanwhile keeps it owed, and the reconciler does it again.
func (r *Runs) settle(ctx context.Context, run store.SOSRun) bool {
	if err := r.syncAlert(ctx, run); err != nil {
		slog.Error("sos: the run's alert work failed; the reconciler finishes it", "error", err, "sos_id", run.SOSID,
			"state", run.State)
		return false
	}
	ok, err := r.store.ClearSOSRunAlertPending(ctx, run.TenantID, run.Origin, run.SOSID, run.Rev)
	if err != nil {
		slog.Warn("sos: clearing the run's owed alert work failed; the reconciler does it again", "error", err, "sos_id", run.SOSID)
	}
	return err == nil && ok
}

// syncAlert makes the run's alert match its state: raised while it is active, closed once it
// is cancelled. Idempotent, keyed by the run's alert id.
func (r *Runs) syncAlert(ctx context.Context, run store.SOSRun) error {
	if run.State == store.SOSRunCancelled {
		return r.closeAlert(ctx, run)
	}
	return r.raise(ctx, run)
}

// Reconcile finishes the alert work runs still owe: an apply whose alert step failed, or a
// replica that stopped between committing a run and raising or closing its alert. Every
// replica runs it; the work is idempotent. It answers how many runs it settled.
func (r *Runs) Reconcile(ctx context.Context) (int, error) {
	runs, err := r.store.ListSOSRunsAlertPending(ctx, 100)
	if err != nil {
		return 0, fmt.Errorf("sos: list owed alert work: %w", err)
	}
	n := 0
	for _, run := range runs {
		if r.settle(ctx, run) {
			n++
			slog.Info("sos: reconciled the run's alert", "tenant", run.TenantID, "sos_id", run.SOSID, "state", run.State)
		}
	}
	return n, nil
}

// RunReconciler reconciles at once (a restart) and then every interval, until ctx ends.
func (r *Runs) RunReconciler(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Reconcile(ctx); err != nil {
			slog.Warn("sos: reconciler", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// chain is the tenant's escalation chain; "" for none.
func (r *Runs) chain(tenantID string) string {
	if r.chainOf == nil {
		return ""
	}
	return r.chainOf(tenantID)
}

// alertFor is the alert a run raises, under its deterministic id.
func alertFor(run store.SOSRun, chain string) *store.Alert {
	subject := run.DeviceID
	if subject == "" || subject == "bridge" {
		subject = run.BridgeID
	}
	text := run.Text
	if text == "" {
		text = fmt.Sprintf("SOS from bridge %s via %s", run.BridgeID, run.Bearers)
	}
	return &store.Alert{ID: AlertID(run.TenantID, run.Origin, run.SOSID), ChainID: chain, DeviceIMEI: subject, Type: "sos", Detail: text}
}

// raise pages for an active run once: the alert id is the run's, so a second replica or a
// copy from another bearer finds it raised. A cancel applied meanwhile closes it again.
// Without an engine or a chain there is nothing to page, and nothing is owed.
func (r *Runs) raise(ctx context.Context, run store.SOSRun) error {
	if r.engine == nil {
		slog.Warn("sos: run active, no escalation engine; logged only", "sos_id", run.SOSID)
		return nil
	}
	chain := r.chain(run.TenantID)
	if chain == "" {
		slog.Warn("sos: run active, no escalation chain configured; alert logged only", "tenant", run.TenantID, "sos_id", run.SOSID)
		return nil
	}
	alert := alertFor(run, chain)
	created, err := r.engine.TriggerOnce(ctx, run.TenantID, alert)
	if err != nil {
		return fmt.Errorf("raise alert: %w", err)
	}
	if created {
		slog.Warn("sos: run paged", "tenant", run.TenantID, "sos_id", run.SOSID, "alert", alert.ID)
	}
	if err := r.store.SetSOSRunAlert(ctx, run.TenantID, run.Origin, run.SOSID, alert.ID); err != nil {
		return fmt.Errorf("record alert: %w", err)
	}
	// A cancel that was applied while the alert was being raised found no alert to close.
	now, err := r.store.GetSOSRun(ctx, run.TenantID, run.Origin, run.SOSID)
	if err != nil {
		return fmt.Errorf("re-read run: %w", err)
	}
	if now.State == store.SOSRunCancelled {
		return r.closeAlert(ctx, *now)
	}
	return nil
}

func (r *Runs) closeAlert(ctx context.Context, run store.SOSRun) error {
	if r.engine == nil {
		return nil
	}
	by := run.CancelledBy
	if by == "" {
		by = run.Origin
	}
	if _, err := r.engine.Cancel(ctx, run.TenantID, AlertID(run.TenantID, run.Origin, run.SOSID), by); err != nil {
		return fmt.Errorf("close alert: %w", err)
	}
	return nil
}

// PageUnstored pages a valid start whose run the database did not take (after retries),
// under the run's own alert id: a copy that stores the run later finds its alert raised and
// pages nobody again. False when even that failed; the caller then pages the old way, as a
// last resort, since an SOS is never dropped.
func (r *Runs) PageUnstored(ctx context.Context, ev Event) bool {
	if r.engine == nil {
		return false
	}
	chain := r.chain(ev.TenantID)
	if chain == "" {
		return true // nothing pages without a chain, the old way neither
	}
	run := store.NewSOSRun(&store.SOSRunEvent{TenantID: ev.TenantID, Origin: Origin(ev.BridgeID), SOSID: ev.SOSID,
		BridgeID: ev.BridgeID, RunStartMs: ev.RunStartMs, Kind: store.SOSEventStart, Bearer: ev.Bearer, DeviceID: ev.DeviceID,
		Text: ev.Text, Lat: ev.Lat, Lon: ev.Lon, HasPosition: ev.HasPosition, At: r.now().UTC()})
	alert := alertFor(run, chain)
	if _, err := r.engine.TriggerOnce(ctx, ev.TenantID, alert); err != nil {
		slog.Error("sos: a start that was not stored could not page under its run's id either", "error", err, "sos_id", ev.SOSID)
		return false
	}
	slog.Warn("sos: a start that was not stored pages under its run's id", "tenant", ev.TenantID, "sos_id", ev.SOSID, "alert", alert.ID)
	return true
}

// RunEvent is what the Hub's own consumers (the live screens, outbound webhooks, TAK) get
// on the SOS topic when a run starts or ends: the old SOSEvent fields plus the run.
type RunEvent struct {
	IMEI     string  `json:"imei"`
	Text     string  `json:"text"`
	Source   string  `json:"source"`
	SOSID    string  `json:"sos_id"`
	State    string  `json:"state"`
	BridgeID string  `json:"bridge_id"`
	Bearers  string  `json:"bearers"`
	Lat      float64 `json:"lat,omitempty"`
	Lon      float64 `json:"lon,omitempty"`
}

func (r *Runs) tell(run store.SOSRun) {
	subject := run.DeviceID
	if subject == "" || subject == "bridge" {
		subject = run.BridgeID
	}
	ev := RunEvent{IMEI: subject, Text: run.Text, Source: "bridge_sos_v2", SOSID: run.SOSID, State: run.State,
		BridgeID: run.BridgeID, Bearers: run.Bearers}
	if run.HasPosition {
		ev.Lat, ev.Lon = run.Lat, run.Lon
	}
	if err := r.bus.PublishJSON(hubmqtt.TopicSOSFor(run.TenantID, subject), 1, false, ev); err != nil {
		slog.Error("sos: publishing the run event failed", "error", err, "sos_id", run.SOSID)
	}
}

// Ack is what a bridge gets on sos/ack for each start and cancel it sent.
type Ack struct {
	V     int    `json:"v"`
	SOSID string `json:"sos_id"`
	Kind  string `json:"kind"` // "start" or "cancel"
	// State is the run's state after the event: a start that arrives after its cancel is
	// answered "cancelled".
	State string `json:"state"`
	Rev   int    `json:"rev"`
}

func (r *Runs) ack(run store.SOSRun, kind string) {
	data, _ := json.Marshal(Ack{V: 2, SOSID: run.SOSID, Kind: kind, State: run.State, Rev: run.Rev})
	if err := r.bus.Publish(hubmqtt.TopicSOSAckFor(run.TenantID, run.BridgeID), 1, false, data); err != nil {
		slog.Warn("sos: ack publish failed; the bridge repeats and is answered again", "error", err, "sos_id", run.SOSID)
	}
}

func (r *Runs) log(ctx context.Context, run store.SOSRun, ev Event, by string) {
	if r.audit == nil {
		return
	}
	action := "sos_run_start"
	if ev.Kind == store.SOSEventCancel {
		action = "sos_run_cancel"
	}
	detail := fmt.Sprintf("sos_id=%s origin=%s bearer=%s state=%s rev=%d", run.SOSID, run.Origin, ev.Bearer, run.State, run.Rev)
	if err := r.audit.Log(ctx, run.TenantID, action, by, detail, ""); err != nil {
		slog.Warn("audit: "+action, "error", err)
	}
}

// MQTTEvent is the v2 JSON a bridge publishes on its {ns}/bridge/{id}/sos/event.
type MQTTEvent struct {
	V          int      `json:"v"`
	Kind       string   `json:"kind"` // "start" or "cancel"
	SOSID      string   `json:"sos_id"`
	RunStartMs *int64   `json:"run_start_ms"`
	DeviceID   string   `json:"device_id,omitempty"`
	Text       string   `json:"text,omitempty"`
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	Reason     int      `json:"reason,omitempty"`
	Timestamp  int64    `json:"timestamp,omitempty"` // unix seconds the bridge sent it
}

// mqttHandlerTimeout bounds one event's store work, as hub_msg does (a stuck database must
// not hold the bus callback); the bridge repeats until it has its ack.
const mqttHandlerTimeout = 10 * time.Second

// HandleMQTT takes a start or cancel from a bridge's sos/event. The bridge id is the topic's,
// which the broker enforced. A start that cannot be taken as its run still pages the old way.
// The work runs off the bus's handler goroutine: it publishes (the ack, the page), and a publish
// waits for a PUBACK the MQTT client cannot read while a handler holds its router.
func (r *Runs) HandleMQTT(topic string, payload []byte) {
	go r.handleMQTT(topic, append([]byte(nil), payload...))
}

func (r *Runs) handleMQTT(topic string, payload []byte) {
	tenantID, bridgeID, ok := hubmqtt.ParseSOSEventTopic(topic)
	if !ok {
		return
	}
	var m MQTTEvent
	err := json.Unmarshal(payload, &m)
	why := ""
	switch {
	case err != nil:
		why = "not JSON: " + err.Error()
	case m.V != 2:
		why = fmt.Sprintf("version %d", m.V)
	case m.RunStartMs == nil:
		why = "no run_start_ms"
	case m.Kind != store.SOSEventStart && m.Kind != store.SOSEventCancel:
		why = "unknown kind " + m.Kind
	}
	ctx, cancel := context.WithTimeout(context.Background(), mqttHandlerTimeout)
	defer cancel()
	if why == "" {
		ev := Event{TenantID: tenantID, BridgeID: bridgeID, Bearer: "mqtt", Kind: m.Kind, SOSID: m.SOSID,
			RunStartMs: *m.RunStartMs, DeviceID: m.DeviceID, Text: m.Text, Reason: m.Reason}
		if m.Lat != nil && m.Lon != nil {
			ev.Lat, ev.Lon, ev.HasPosition = *m.Lat, *m.Lon, true
		}
		if _, err := r.Apply(ctx, ev); err != nil {
			if !errors.Is(err, ErrBadSOSID) {
				// Valid, but not stored (the database): no ack, so the bridge publishes it
				// again until it has one. Not paged the old way, under a second identity.
				slog.Error("sos: event not stored; the bridge repeats it until its ack", "error", err, "tenant", tenantID,
					"bridge_id", bridgeID, "kind", m.Kind, "sos_id", m.SOSID)
				return
			}
			why = err.Error()
		}
	}
	if why == "" {
		return
	}
	if m.Kind != store.SOSEventStart {
		slog.Warn("sos: event refused; nothing changed", "why", why, "tenant", tenantID, "bridge_id", bridgeID, "kind", m.Kind)
		return
	}
	// Never drop an SOS: page the old way, under the old per-bridge, per-second claim.
	slog.Warn("sos: start not taken as its run; paging the old way", "why", why, "tenant", tenantID, "bridge_id", bridgeID)
	ts := r.now().UTC()
	if m.Timestamp > 0 {
		ts = time.Unix(m.Timestamp, 0).UTC()
	}
	subject := m.DeviceID
	if subject == "" || subject == "bridge" {
		subject = bridgeID
	}
	text := m.Text
	if text == "" {
		text = fmt.Sprintf("SOS from bridge %s via mqtt", bridgeID)
	}
	legacy := map[string]any{
		"id": fmt.Sprintf("sos-mqtt-%s-%d", bridgeID, ts.Unix()), "imei": subject, "bridge_id": bridgeID, "text": text,
		"sos": true, "channel": "mqtt", "source": "bridge_sos", "timestamp": ts.Format(time.RFC3339),
	}
	if m.Lat != nil && m.Lon != nil {
		legacy["lat"], legacy["lon"] = *m.Lat, *m.Lon
	}
	if err := r.bus.PublishJSON(hubmqtt.TopicMODecodedFor(tenantID, subject), 1, false, legacy); err != nil {
		slog.Error("sos: the old-way page could not be published", "error", err, "bridge_id", bridgeID)
	}
}

// Caps is what the Hub tells a bridge on its retained hub/caps at every birth.
type Caps struct {
	V    int      `json:"v"`
	Caps []string `json:"caps"`
}

// CapSOSV2 says the Hub takes 0x07/0x08 frames and sos/event v2 JSON (C10): a bridge sends
// them only while its Hub says so, and the old frames otherwise.
const CapSOSV2 = "sos_v2"

// PublishCaps tells one bridge what this Hub understands. Retained, so a bridge that
// connects before the Hub has handled its birth reads it too.
func PublishCaps(b bus.MessageBus, tenantID, bridgeID string, sosV2 bool) error {
	c := Caps{V: 1, Caps: []string{}}
	if sosV2 {
		c.Caps = append(c.Caps, CapSOSV2)
	}
	return b.PublishJSON(hubmqtt.TopicHubCapsFor(tenantID, bridgeID), 1, true, c)
}
