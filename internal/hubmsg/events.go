package hubmsg

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The scopes the Hub writes on its events.
const (
	scopeHub       = "the Hub"
	scopeRecipient = "the recipient's MeshSat app"
)

// EventIDFor is the id of one kind of evidence about one operation: a UUIDv7 whose time is the
// operation's creation and whose other bits come from the operation's key and the evidence.
// Every replica that records the same evidence makes the same id, so it is stored once.
func EventIDFor(op *store.MsgOp, kind, class, reason string) string {
	h := sha256.New()
	for _, f := range []string{op.TenantID, op.Sender, op.OpID, kind, class, reason} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(min(uint64(len(f)), math.MaxUint32))) // #nosec G115 -- clamped; ids and codes are far shorter
		h.Write(n[:])
		h.Write([]byte(f))
	}
	sum := h.Sum(nil)
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(max(op.CreatedAt.UnixMilli(), 0))) // #nosec G115 -- clamped to 0 and up
	copy(b[0:6], ms[2:8])
	copy(b[6:], sum[:10])
	b[6] = 0x70 | b[6]&0x0F
	b[8] = 0x80 | b[8]&0x3F
	return format(b)
}

// NewEvent is the store row of one piece of evidence the Hub records about op.
func NewEvent(op *store.MsgOp, kind, class, authority, reason, detail string, at time.Time) *store.MsgOpEvent {
	scope := scopeHub
	if authority == "recipient" {
		scope = scopeRecipient
	}
	return &store.MsgOpEvent{
		TenantID: op.TenantID, Sender: op.Sender, OpID: op.OpID, EventID: EventIDFor(op, kind, class, reason),
		Class: class, Kind: kind, Reason: reason, Authority: authority, Scope: scope, Detail: detail, At: at.UTC(),
	}
}

// Wire is the event as the sender's msg/status carries it.
func Wire(op *store.MsgOp, e store.MsgOpEvent) Event {
	return Event{
		V: 1, OpID: e.OpID, Mid: op.Mid, Recipient: op.Recipient, EventID: e.EventID, Rev: e.Rev, Class: e.Class,
		Kind: e.Kind, Reason: e.Reason, Authority: e.Authority, Scope: e.Scope, Detail: e.Detail,
		At: e.At.UTC().Format(time.RFC3339),
	}
}

// Project is the store's projection callback for op: its events as the Hub holds them, which
// all came from the Hub's own processing (the status-topic source of their authority).
func Project(op *store.MsgOp) func([]store.MsgOpEvent) (string, bool) {
	ref := Operation{OpID: op.OpID, Mid: op.Mid, Recipient: op.Recipient}
	return func(evs []store.MsgOpEvent) (string, bool) {
		wire := make([]Event, 0, len(evs))
		for _, e := range evs {
			w := Wire(op, e)
			w.Source = SourceOf[e.Authority]
			wire = append(wire, w)
		}
		p := Reduce(ref, wire)
		return p.State, p.Conflict
	}
}

// Refusal is the Hub's answer to a submission it did not admit, on the sender's msg/status.
// It is not evidence of any operation (an op_conflict refusal concerns a submission, not the
// stored operation under that key), so a party's reducer never takes it in; the type field
// tells it apart from an event.
type Refusal struct {
	V      int    `json:"v"`
	Type   string `json:"type"` // "refusal"
	OpID   string `json:"op_id"`
	Mid    string `json:"mid"`
	Reason string `json:"reason"`
}

// Delivery is what the Hub publishes on the recipient's msg/in: the submission's fields and the
// principal it came from, the origin of the recipient's commit key (C3).
type Delivery struct {
	V             int    `json:"v"`
	OpID          string `json:"op_id"`
	Mid           string `json:"mid"`
	From          string `json:"from"`
	To            string `json:"to"`
	ContentType   string `json:"content_type"`
	MidExpiresAt  int64  `json:"mid_expires_at"`
	ExpiresAt     int64  `json:"expires_at"`
	ContentSHA256 string `json:"content_sha256"`
	Body          string `json:"body"`
}

// DeliveryOf is op's msg/in payload.
func DeliveryOf(op *store.MsgOp) Delivery {
	return Delivery{
		V: 1, OpID: op.OpID, Mid: op.Mid, From: op.Sender, To: op.Recipient, ContentType: op.ContentType,
		MidExpiresAt: op.MidExpiresAt, ExpiresAt: op.ExpiresAt, ContentSHA256: op.ContentSHA256, Body: string(op.Body),
	}
}

// Ack is the recipient's answer on its own msg/ack (C5), naming the origin it committed under.
type Ack struct {
	V      int    `json:"v"`
	OpID   string `json:"op_id"`
	Mid    string `json:"mid"`
	From   string `json:"from"`
	Result string `json:"result"` // "stored" or "refused"
	Reason string `json:"reason"`
}

// The reasons a recipient may refuse with (C5).
var recipientReasons = map[string]bool{
	"op_conflict": true, "mid_conflict": true, "digest_mismatch": true, "bad_expiry": true, "bad_id": true,
	"expired_at_recipient": true, "receiver_busy": true,
}

// AckEvidence maps a recipient's answer to the evidence it is (C6a) and the answer the outbox
// records; answer "" keeps the outbox delivering (a busy recipient). ok is false for an answer
// that is not one of C5's.
func AckEvidence(a Ack) (kind, class, reason, answer string, ok bool) {
	switch {
	case a.Result == "stored" && a.Reason == "":
		return "recipient_app_stored", "completed", "", "stored", true
	case a.Result == "refused" && a.Reason == "receiver_busy":
		return "recipient_busy", "unknown", "receiver_busy", "", true
	case a.Result == "refused" && a.Reason == "expired_at_recipient":
		return "recipient_refused", "expired", a.Reason, "refused", true
	case a.Result == "refused" && recipientReasons[a.Reason]:
		return "recipient_refused", "failed", a.Reason, "refused", true
	}
	return "", "", "", "", false
}
