package store

import "time"

// RNSPath is one row of the Hub's Reticulum path table (MESHSAT-1555),
// persisted so the node that takes over after a leader failover answers path
// requests and routes to known destinations without waiting for every peer
// to announce again. It is the Bridge's rns_paths shape.
//
// Platform-wide by nature: a Reticulum destination hash carries no tenant,
// and the node is one transport for every tenant's clients. Nothing in a row
// is secret (an announce is public by definition), so the table is not in
// RedactedInExport and, having no tenant_id, is not part of a tenant export
// or purge.
type RNSPath struct {
	DestHash string `json:"dest_hash"` // 32 hex characters
	NextHop  string `json:"next_hop"`  // 32 hex characters, == dest_hash when direct
	Hops     int    `json:"hops"`
	// Iface is the interface id the path was learned on. A TCP connection's
	// id does not survive a restart, so a reloaded path may name an interface
	// that is gone until the peer announces again.
	Iface     string    `json:"iface"`
	ExpiresAt time.Time `json:"expires_at"`
	// RandomBlobs is the concatenation of the 10-byte random hashes heard for
	// this destination, newest last; AnnounceRaw the cached announce packet
	// a path response is answered with.
	RandomBlobs []byte    `json:"-"`
	AnnounceRaw []byte    `json:"-"`
	UpdatedAt   time.Time `json:"updated_at"`
}
