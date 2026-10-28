package store

import "time"

// LXMFAttachment is one voice note or photo an LXMF message in a tenant's
// inbox or outbox carried in its fields (MESHSAT-1735): the metadata here,
// keyed (tenant_id, message_id, idx) where idx is the attachment's place in
// the message (the image first); the bytes in the object store under
// ObjectKey. A tenant's attachments are its own, as its messages are.
type LXMFAttachment struct {
	TenantID  string `json:"tenant_id"`
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	// Kind is audio or image; Mode the LXMF audio mode of a note (Codec2
	// 0x01-0x09, 700C = 3, Opus 0x10-0x19); Format the image format ("webp").
	Kind      string    `json:"kind"`
	Mode      int       `json:"mode,omitempty"`
	Format    string    `json:"format,omitempty"`
	Size      int       `json:"size"`
	ObjectKey string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}
