package store

import (
	"strings"
	"time"
)

// SOS runs: one record per SOS a sender raised under its own identity (HeMB-NG stage 2, C10,
// MESHSAT-1584), keyed (tenant, origin principal, sos_id). The record is what makes an SOS
// page once whichever bearers carry it, and what makes a cancel terminal: a cancel stores
// the run as cancelled even when its start has not arrived, and a start that arrives after
// it is recorded without paging. The contract is docs/bridge-hub-protocol.md, "SOS
// identity (C10)"; the shared cases are docs/fixtures/v1/sos.json.

// SOS run states.
const (
	SOSRunActive    = "active"
	SOSRunCancelled = "cancelled"
)

// SOSRun is the Hub's record of one SOS.
type SOSRun struct {
	TenantID string `json:"tenant_id"`
	// Origin is the principal that raised it, "bridge:<id>": the bridge the ingress
	// authenticated (the topic over MQTT, the modem's bridge over satellite).
	Origin   string `json:"origin"`
	SOSID    string `json:"sos_id"`
	BridgeID string `json:"bridge_id"`
	// RunStartMs is the sender's start of the run, carried in every frame and event.
	RunStartMs int64  `json:"run_start_ms"`
	State      string `json:"state"`
	// Rev counts the events applied to the run, duplicates included.
	Rev int `json:"rev"`
	// Bearers lists every bearer a start or cancel arrived over, in order, each once.
	Bearers     string  `json:"bearers"`
	DeviceID    string  `json:"device_id,omitempty"`
	Text        string  `json:"text,omitempty"`
	Lat         float64 `json:"lat,omitempty"`
	Lon         float64 `json:"lon,omitempty"`
	HasPosition bool    `json:"has_position"`
	// AlertID is the escalation alert the run raised; empty until one exists.
	AlertID string `json:"alert_id,omitempty"`
	// AlertPending is the alert work the run's state still owes (raise it while active,
	// close it once cancelled), set in the same transaction as the state change and cleared
	// only once that work succeeded for the run's rev. The reconciler finishes what an
	// apply could not, so an ack the sender got always stands for durable work.
	AlertPending bool `json:"alert_pending"`
	// StartedAt is when the first start arrived; nil when only the cancel has.
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	// CancelledBy is the principal that cancelled: the origin, or "user:<id>" for an
	// operator of the tenant.
	CancelledBy string `json:"cancelled_by,omitempty"`
	// CancelReason is the frame's reason (0 operator, 1 test ended); -1 when not cancelled.
	CancelReason int       `json:"cancel_reason"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SOS run event kinds.
const (
	SOSEventStart  = "start"
	SOSEventCancel = "cancel"
)

// SOSRunEvent is one start or cancel to apply to a run.
type SOSRunEvent struct {
	TenantID, Origin, SOSID, BridgeID string
	RunStartMs                        int64
	Kind                              string // SOSEventStart or SOSEventCancel
	Bearer                            string
	// A start's content; ignored for a cancel.
	DeviceID, Text string
	Lat, Lon       float64
	HasPosition    bool
	// A cancel's author and reason; ignored for a start.
	By     string
	Reason int
	At     time.Time
}

// SOSRunApplied is what applying one event did.
type SOSRunApplied struct {
	Run SOSRun
	// Created is true when this event made the record.
	Created bool
	// Ended is true when this event cancelled a run that was active: its alert must stop.
	Ended bool
}

// NewSOSRun is the record an event makes when its run is unknown: active for a start,
// cancelled for a cancel (terminal before its start).
func NewSOSRun(ev *SOSRunEvent) SOSRun {
	at := ev.At.UTC()
	r := SOSRun{
		TenantID: ev.TenantID, Origin: ev.Origin, SOSID: ev.SOSID, BridgeID: ev.BridgeID,
		RunStartMs: ev.RunStartMs, Rev: 1, Bearers: AddSOSBearer("", ev.Bearer), CancelReason: -1,
		CreatedAt: at, UpdatedAt: at,
	}
	// Either way the alert work is owed: a start raises the alert; a cancel that arrived
	// first closes one a fallback page may have raised under the run's id.
	r.AlertPending = true
	if ev.Kind == SOSEventCancel {
		r.State = SOSRunCancelled
		r.CancelledAt, r.CancelledBy, r.CancelReason = &at, ev.By, ev.Reason
		return r
	}
	r.State = SOSRunActive
	r.StartedAt = &at
	r.DeviceID, r.Text, r.Lat, r.Lon, r.HasPosition = ev.DeviceID, ev.Text, ev.Lat, ev.Lon, ev.HasPosition
	return r
}

// ApplySOSEvent applies an event to a known run and reports whether it ended an active
// run. A start adds its bearer and, when only the cancel had arrived, the start's time and
// content; it never makes a cancelled run active. A cancel ends an active run and is
// otherwise recorded by its bearer. Every event counts in Rev.
func ApplySOSEvent(r *SOSRun, ev *SOSRunEvent) (ended bool) {
	at := ev.At.UTC()
	r.Rev++
	r.Bearers = AddSOSBearer(r.Bearers, ev.Bearer)
	r.UpdatedAt = at
	switch ev.Kind {
	case SOSEventStart:
		if r.StartedAt == nil {
			r.StartedAt = &at
			r.DeviceID, r.Text, r.Lat, r.Lon, r.HasPosition = ev.DeviceID, ev.Text, ev.Lat, ev.Lon, ev.HasPosition
		}
	case SOSEventCancel:
		if r.State == SOSRunActive {
			r.State = SOSRunCancelled
			r.CancelledAt, r.CancelledBy, r.CancelReason = &at, ev.By, ev.Reason
			r.AlertPending = true
			return true
		}
	}
	return false
}

// AddSOSBearer appends bearer to the comma-separated list when it is not there yet.
func AddSOSBearer(list, bearer string) string {
	if bearer == "" {
		return list
	}
	for _, b := range strings.Split(list, ",") {
		if b == bearer {
			return list
		}
	}
	if list == "" {
		return bearer
	}
	return list + "," + bearer
}
