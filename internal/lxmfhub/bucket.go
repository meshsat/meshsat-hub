package lxmfhub

import "time"

// bucket is a token bucket: capacity tokens, refilled at rate per second.
// One per tenant guards the inbox against a peer that floods an address;
// the node runs on one replica, so in-memory is exact.
type bucket struct {
	tokens, capacity, rate float64
	last                   time.Time
}

func newBucket(perMinute int, now time.Time) *bucket {
	c := float64(perMinute)
	return &bucket{tokens: c, capacity: c, rate: c / 60, last: now}
}

func (b *bucket) allow(now time.Time) bool {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
