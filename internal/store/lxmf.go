package store

import "time"

// LXMF message states. An inbound message is received; an outbound one is
// queued by the API, sending once the leader has claimed it, then delivered
// on the peer's proof or failed (MESHSAT-1555).
const (
	LXMFStateReceived  = "received"
	LXMFStateQueued    = "queued"
	LXMFStateSending   = "sending"
	LXMFStateDelivered = "delivered"
	LXMFStateFailed    = "failed"

	LXMFDirectionIn  = "in"
	LXMFDirectionOut = "out"
)

// LXMFMessage is one LXMF message in a tenant's inbox or outbox. Keyed
// (tenant_id, id): a tenant's address is its own and nothing about another
// tenant's traffic is visible through it.
type LXMFMessage struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// Hash is the LXMF message hash (32 bytes hex), known for every inbound
	// message and for an outbound one once it was packed.
	Hash      string `json:"hash,omitempty"`
	Direction string `json:"direction"`
	State     string `json:"state"`
	// Source and Destination are lxmf.delivery hashes (16 bytes hex).
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Title       string `json:"title,omitempty"`
	Content     string `json:"content"`
	// FieldsJSON is the message's LXMF fields rendered as JSON (binary
	// values base64), "" when there are none.
	FieldsJSON string `json:"fields_json,omitempty"`
	// Method is how it travelled: opportunistic, direct or "" (not yet).
	Method         string `json:"method,omitempty"`
	StampValue     int    `json:"stamp_value,omitempty"`
	SignatureValid bool   `json:"signature_valid"`
	// Detail carries the failure reason of a failed send.
	Detail         string    `json:"detail,omitempty"`
	CreatedByEmail string    `json:"created_by_email,omitempty"`
	MessageAt      time.Time `json:"message_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Attachments is how many voice notes and photos the message carries
	// (lxmf_attachments rows), counted by the store on every read, so a
	// listing says which rows have something to fetch and the console
	// asks for nothing of the rest (MESHSAT-1735).
	Attachments int `json:"attachments"`
}

// LXMFStateUpdate moves an outbound message along: State is required; Hash,
// Method and Detail are written when non-empty.
type LXMFStateUpdate struct {
	State  string
	Hash   string
	Method string
	Detail string
}

// LXMFQuery filters a listing; zero values mean no filter. Limit 0 means 50.
type LXMFQuery struct {
	Direction string
	State     string
	Limit     int
	Before    time.Time
}
