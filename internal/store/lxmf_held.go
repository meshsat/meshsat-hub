package store

import "time"

// LXMFHeldMessage is one message a tenant's LXMF propagation node holds for
// a client that is offline (MESHSAT-1555 P5). The node cannot read it: Data
// is the destination hash and the payload encrypted to that destination,
// exactly as the client handed it over, without the trailing stamp. Keyed
// (tenant_id, id) where id is the transient id, SHA-256 of Data, hex.
type LXMFHeldMessage struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// Destination is the lxmf.delivery hash the message is for (16 bytes hex).
	Destination string `json:"destination"`
	Data        []byte `json:"-"`
	Size        int    `json:"size"`
	StampValue  int    `json:"stamp_value"`
	// Source is the identity hash of the client that handed it over, hex,
	// "" when it did not identify.
	Source     string    `json:"source,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}
