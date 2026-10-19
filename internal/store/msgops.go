package store

import "time"

// MsgOp is one operation of the Hub app route hub_msg (HeMB-NG stage 2, contract C4,
// MESHSAT-1584): a sender's attempt to have one message (Mid) stored by one recipient. It is
// keyed (TenantID, Sender, OpID), the principal the broker enforced on the topic, with an
// index (TenantID, Sender, Mid). The row is also the outbox entry that delivers it: the
// leader publishes it on the recipient's msg/in until the recipient answers or ExpiresAt.
type MsgOp struct {
	TenantID string `json:"tenant_id"`
	// Sender and Recipient are principals, "bridge:<id>" in stage 2.
	Sender        string `json:"sender"`
	OpID          string `json:"op_id"`
	Mid           string `json:"mid"`
	Recipient     string `json:"recipient"`
	ContentType   string `json:"content_type"`
	MidExpiresAt  int64  `json:"mid_expires_at"` // unix ms
	ExpiresAt     int64  `json:"expires_at"`     // unix ms
	ContentSHA256 string `json:"content_sha256"`
	Body          []byte `json:"-"`

	// State is the projection of the operation's evidence (C6): pending, accepted,
	// unknown, forwarded, expired, failed or completed. Conflict marks a set that holds
	// both completed and failed. Rev is the number of the newest event.
	State    string `json:"state"`
	Conflict bool   `json:"conflict"`
	Rev      int    `json:"rev"`

	// The outbox: what the drainer did, and when it may try again. RecipientAnswer is
	// "" until the recipient acknowledged (stored or refused); a busy recipient leaves it
	// empty so the Hub keeps trying until ExpiresAt.
	Attempts        int       `json:"attempts"`
	NextAttemptAt   time.Time `json:"next_attempt_at"`
	LeasedUntil     time.Time `json:"-"`
	RecipientAnswer string    `json:"recipient_answer,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MsgOpTerminal reports whether an operation's state is final for the outbox.
func MsgOpTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "expired"
}

// MsgOpEvent is one piece of evidence about an operation (C6), as the Hub publishes it on the
// sender's msg/status. EventID is a UUIDv7 and unique across all operations.
type MsgOpEvent struct {
	TenantID  string    `json:"tenant_id"`
	Sender    string    `json:"sender"`
	OpID      string    `json:"op_id"`
	EventID   string    `json:"event_id"`
	Rev       int       `json:"rev"`
	Class     string    `json:"class"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason"`
	Authority string    `json:"authority"`
	Scope     string    `json:"scope"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at"`
	// PublishedAt is when the sender's msg/status last took it; nil until then, and again
	// after the sender repeated its submission (every event is replayed, C4).
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// MsgOpAdmission is what AdmitMsgOp found.
type MsgOpAdmission struct {
	// Outcome is MsgOpAdmitted (the new operation is stored), MsgOpExisting (Existing has
	// the stored one under the same key) or MsgOpMidConflict (the sender's Mid is known
	// with another content digest; nothing was stored).
	Outcome  string
	Existing *MsgOp
}

// The outcomes of AdmitMsgOp.
const (
	MsgOpAdmitted    = "admitted"
	MsgOpExisting    = "existing"
	MsgOpMidConflict = "mid_conflict"
)
