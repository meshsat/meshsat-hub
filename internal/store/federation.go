package store

import "time"

// FederationPeer is another Hub tenant this tenant's owner has added by its
// certificate (MESHSAT-1549). A peering is mutual by construction: this Hub
// sends only to peers it holds and accepts only from them, and the other Hub
// does the same, so traffic flows only when both owners added each other.
//
// Keyed (tenant_id, id). RelayURL is the PEER's capability URL
// (/api/webhook/federation/<token>) and is in RedactedInExport: the token in
// it is part of the peer's defence, even though every request is also signed.
type FederationPeer struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	RelayURL string `json:"relay_url"`
	// CertPEM is the peer's self-signed federation certificate; only its
	// Ed25519 key is used. Fingerprint is its SPKI SHA-256 (tlspin.SPKIHash),
	// unique per tenant, and is what the owners compare out of band.
	CertPEM        string    `json:"cert_pem"`
	Fingerprint    string    `json:"fingerprint"`
	CreatedByEmail string    `json:"created_by_email,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// FederationGrant lets one modem of a peer reach one device of this tenant
// over federation: a relayed message from FromIMEI on PeerID to ToIMEI is
// sent as an MT on this tenant's own Cloudloop account, so a grant is a
// standing spend decision of this tenant's owner. Keyed (tenant_id, id).
type FederationGrant struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	PeerID         string    `json:"peer_id"`
	FromIMEI       string    `json:"from_imei"`
	ToIMEI         string    `json:"to_imei"`
	CreatedByEmail string    `json:"created_by_email,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
