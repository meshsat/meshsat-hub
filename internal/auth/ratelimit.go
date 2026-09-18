package auth

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// windowCounter is a fixed one-minute window per key. It is deliberately the
// same simple shape as the per-IP webhook limiter: enough to stop a runaway
// client or a scripted crawl, cheap enough to sit on every request.
type windowCounter struct {
	n         int
	windowEnd time.Time
}

type budget struct {
	mu       sync.Mutex
	counters map[string]*windowCounter
	rpm      int
}

func newBudget(rpm int) *budget {
	b := &budget{counters: map[string]*windowCounter{}, rpm: rpm}
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for range t.C {
			now := time.Now()
			b.mu.Lock()
			for k, c := range b.counters {
				if now.After(c.windowEnd) {
					delete(b.counters, k)
				}
			}
			b.mu.Unlock()
		}
	}()
	return b
}

// take records one request for key and reports whether it is still within the
// budget, plus the seconds left in the window (for Retry-After).
func (b *budget) take(key string) (ok bool, retryAfter int) {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	c, found := b.counters[key]
	if !found || now.After(c.windowEnd) {
		c = &windowCounter{windowEnd: now.Add(time.Minute)}
		b.counters[key] = c
	}
	c.n++
	if c.n > b.rpm {
		return false, int(time.Until(c.windowEnd).Seconds()) + 1
	}
	return true, 0
}

// PrincipalRateLimit budgets AUTHENTICATED requests per principal -- the user
// id or the API key id the auth middleware resolved -- at rpm per minute
// (ASVS V4, MESHSAT-1219). It must run after authentication.
//
// A request that carries no principal is not touched: that is every
// auth-exempt path -- the satellite and SMS webhooks, the health probes, the
// capability URLs -- which have budgets of their own where they need them and
// must never be throttled by this one (an SOS arrives on a webhook). rpm <= 0
// disables the budget.
func PrincipalRateLimit(rpm int) func(http.Handler) http.Handler {
	b := newBudget(rpm)
	return func(next http.Handler) http.Handler {
		if rpm <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := FromContext(r.Context())
			if u == nil || u.ID == "" {
				next.ServeHTTP(w, r)
				return
			}
			if ok, retry := b.take(u.ID); !ok {
				metrics.RateLimitTripsTotal.WithLabelValues("principal").Inc()
				slog.Warn("ratelimit: principal budget exceeded",
					"principal", u.ID, "tenant", u.TenantID, "ip", clientIPOrUnknown(r),
					"method", r.Method, "path", r.URL.Path, "rpm", rpm)
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limited"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// apiKeyFailureBudget cuts off an address that keeps presenting invalid API
// keys: after apiKeyFailuresPerMin failures in a minute the middleware refuses
// without hashing or looking the key up, so a guessing loop costs the database
// nothing. The answer is the same 401 as a wrong key -- an attacker learns
// nothing from the cut-off -- but it is counted and logged separately.
const apiKeyFailuresPerMin = 20

var apiKeyFailures = newBudget(apiKeyFailuresPerMin)

// apiKeyFailureExceeded reports whether ip is over its invalid-key budget.
// Only failures are recorded (see APIKeyMiddleware), so a valid key never
// counts against its own address.
func apiKeyFailureExceeded(ip string) bool {
	apiKeyFailures.mu.Lock()
	c, found := apiKeyFailures.counters[ip]
	over := found && time.Now().Before(c.windowEnd) && c.n >= apiKeyFailuresPerMin
	apiKeyFailures.mu.Unlock()
	return over
}

func recordAPIKeyFailure(ip string) {
	_, _ = apiKeyFailures.take(ip)
}
