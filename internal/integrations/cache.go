package integrations

import "sync"

// ClientCache keeps one built client per tenant, rebuilt when the account
// fingerprint changes. Provider packages use it for their per-tenant pools.
type ClientCache[T any] struct {
	mu      sync.Mutex
	entries map[string]cacheEntry[T]
}

type cacheEntry[T any] struct {
	fp string
	v  T
}

// Get returns the cached client for the tenant when its fingerprint still
// matches, otherwise builds, stores and returns a new one.
func (c *ClientCache[T]) Get(tenantID, fp string, build func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cacheEntry[T]{}
	}
	if e, ok := c.entries[tenantID]; ok && e.fp == fp {
		return e.v
	}
	v := build()
	c.entries[tenantID] = cacheEntry[T]{fp: fp, v: v}
	return v
}

// Fingerprint joins account fields into a cache key.
func Fingerprint(parts ...string) string {
	out := ""
	for _, p := range parts {
		out += p + "\x00"
	}
	return out
}

// OwnsDevice reports whether the tenant that authenticated a webhook (want)
// may act for the device: true when it owns it or the device is unregistered
// (it then joins want). ownerOf resolves the owner given the hint.
func OwnsDevice(want string, ownerOf func(hint string) string) bool {
	if want == "" {
		return true
	}
	return ownerOf(want) == want
}
