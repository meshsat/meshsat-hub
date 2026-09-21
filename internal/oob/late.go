package oob

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// A command over a satellite bearer can outlive the Hub's patience
// (MESHSAT-1293). The MT waits at Iridium for the kit's next pass, which can be
// an hour away, while the Hub gives up after its bearer timeout and tells the
// caller the command failed. On 21 Sep 2026 a PING reached the kit 69 minutes
// after the Hub had answered 504, ran, and replied; the reply was logged as an
// ordinary one.
//
// Two things follow. A request carries an expiry (Frame.ExpiresAt, spec v1.2
// section 3.1) so a bridge with a set clock refuses it once the Hub has stopped
// waiting. And for the bridges that still run it (older software, a clock that
// is not set, or the minutes of clock allowance) the Hub remembers what it gave
// up on, so the reply that turns up afterwards is reported as exactly that.
//
// The record lives in the store the replicas share, not in this process: the
// reply lands on whichever replica takes the webhook, often not the one that
// sent, and a rollout between the send and the pass is likely at these delays.

// LateStore is the shared key-value store the given-up records live in. The
// command job store's KV (internal/cmdjobs) satisfies it.
type LateStore interface {
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
}

// lateRetention is how long a given-up command is remembered. A satellite MT
// that waits longer than this for a pass is past anybody's interest.
const lateRetention = 7 * 24 * time.Hour

// GaveUp is the record of a command the Hub stopped waiting for.
type GaveUp struct {
	Counter   uint32    `json:"counter"`
	Cmd       string    `json:"cmd"`
	Bearer    string    `json:"bearer"`
	SentAt    time.Time `json:"sent_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	GaveUpAt  time.Time `json:"gave_up_at"`
}

// SetLateStore attaches the shared store. Without one, late replies are logged
// as ordinary replies, as they were before.
func (s *Service) SetLateStore(ls LateStore) {
	s.mu.Lock()
	s.late = ls
	s.mu.Unlock()
}

// lateKey names a record by the reply's correlation: the reply carries only the
// low 16 bits of the request counter, the same key the waiters use.
func lateKey(tenantID, bridgeID string, counterLo uint32) string {
	return fmt.Sprintf("ooblate:%s:%s:%d", tenantID, bridgeID, counterLo&0xFFFF)
}

func (s *Service) lateStore() LateStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.late
}

// recordGaveUp remembers that the Hub stopped waiting for a command. It runs
// on the way out of Send, whose context may already be cancelled.
func (s *Service) recordGaveUp(ctx context.Context, tenantID, bridgeID string, g GaveUp) {
	ls := s.lateStore()
	if ls == nil {
		return
	}
	body, err := json.Marshal(g)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := ls.Set(wctx, lateKey(tenantID, bridgeID, g.Counter), body, lateRetention); err != nil {
		slog.Warn("oob: could not record a command the Hub gave up on; a late reply to it will look ordinary",
			"bridge", bridgeID, "counter", g.Counter, "error", err)
	}
}

// gaveUpOn returns the record for a reply's request, or nil when the Hub was
// still waiting for it (or never sent it, or the store is unavailable).
func (s *Service) gaveUpOn(ctx context.Context, tenantID, bridgeID string, counterLo uint32) *GaveUp {
	ls := s.lateStore()
	if ls == nil {
		return nil
	}
	raw, found, err := ls.Get(ctx, lateKey(tenantID, bridgeID, counterLo))
	if err != nil || !found {
		return nil
	}
	var g GaveUp
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	return &g
}

// timeoutError is what the caller of a timed-out command is told. The command
// has left the Hub and may still arrive: saying only "no reply" read as "it
// did not happen", and on 21 Sep 2026 it had not happened YET.
func timeoutError(bearer string, waited time.Duration, expires time.Time, gotSegments, total int) error {
	var partial string
	if gotSegments > 0 {
		partial = fmt.Sprintf(": %d of %d segments arrived", gotSegments, total)
	}
	return fmt.Errorf("%w (%s, %s%s). It may still reach the bridge; a bridge with a set clock refuses it after %s, an older one may still run it",
		ErrTimeout, bearer, waited, partial, expires.UTC().Format("15:04:05Z"))
}
