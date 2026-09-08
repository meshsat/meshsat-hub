package dbwrap

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ErrRetryExhausted wraps the last transient error once a bounded retry loop
// gives up. Callers can test for it with errors.Is.
var ErrRetryExhausted = errors.New("dbwrap: retry attempts exhausted")

// defaultMaxAttempts bounds every retry loop. It is a package-level default
// because ObservedDB instances are created inside the store constructors;
// main sets it from HUB_DB_RETRY_MAX_ATTEMPTS before the stores are opened.
var defaultMaxAttempts atomic.Int32

func init() {
	defaultMaxAttempts.Store(8)
}

// SetDefaultMaxAttempts sets the attempt bound used by every ObservedDB created
// afterwards. Values below 1 are ignored.
func SetDefaultMaxAttempts(n int) {
	if n >= 1 {
		defaultMaxAttempts.Store(int32(n)) // #nosec G115 -- bounded config value
	}
}

// DefaultMaxAttempts returns the current attempt bound.
func DefaultMaxAttempts() int {
	return int(defaultMaxAttempts.Load())
}

// sqlStater is implemented by Postgres driver errors (pgconn.PgError,
// lib/pq Error). Matching the method instead of the type keeps this package
// free of a Postgres driver import.
type sqlStater interface {
	SQLState() string
}

// Retry reasons. They label the transient-retry metric and choose the backoff
// policy: lock conflicts are retried fast and few, connectivity/readiness
// errors slow and long.
const (
	ReasonWSREP      = "wsrep_1047"    // Galera node not ready for application use
	ReasonDeadlock   = "deadlock"      // MySQL 1213, Postgres 40P01
	ReasonLockWait   = "lock_wait"     // MySQL 1205
	ReasonSerialize  = "serialization" // Postgres 40001
	ReasonConnection = "connection"    // driver.ErrBadConn, Postgres class 08
	ReasonPGShutdown = "pg_shutdown"   // Postgres 57P01/57P02 (CNPG switchover)
	ReasonPGStarting = "pg_starting"   // Postgres 57P03 (cannot connect now)
	ReasonPGReadOnly = "pg_read_only"  // Postgres 25006 (talking to a demoted primary)
)

// IsTransient classifies an error. ok is true when the operation may succeed
// on retry; reason labels the metric and selects the backoff policy.
func IsTransient(err error) (reason string, ok bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return ReasonConnection, true
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1047:
			return ReasonWSREP, true
		case 1213:
			return ReasonDeadlock, true
		case 1205:
			return ReasonLockWait, true
		}
		return "", false
	}
	var st sqlStater
	if errors.As(err, &st) {
		code := st.SQLState()
		switch {
		case code == "40001":
			return ReasonSerialize, true
		case code == "40P01":
			return ReasonDeadlock, true
		case code == "57P01" || code == "57P02":
			return ReasonPGShutdown, true
		case code == "57P03":
			return ReasonPGStarting, true
		case code == "25006":
			return ReasonPGReadOnly, true
		case strings.HasPrefix(code, "08"):
			return ReasonConnection, true
		}
	}
	return "", false
}

// IsRetryable reports whether err is a transient database error.
func IsRetryable(err error) bool {
	_, ok := IsTransient(err)
	return ok
}

// policy describes how a reason is retried.
type policy struct {
	base time.Duration
	cap  time.Duration
}

func policyFor(reason string) policy {
	switch reason {
	case ReasonDeadlock, ReasonLockWait, ReasonSerialize:
		return policy{base: 50 * time.Millisecond, cap: time.Second}
	default:
		return policy{base: time.Second, cap: 30 * time.Second}
	}
}

// Backoff returns the delay before retry number attempt (0-indexed) under the
// given policy: base doubled per attempt with a shift (never a float power,
// which overflowed int64 at attempt 63 and produced the negative "backoff:0"
// hot loop of Incident 20a), capped, with up to 20% jitter added.
func Backoff(attempt int, p policy) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 20 {
		attempt = 20
	}
	d := p.base << uint(attempt) // #nosec G115 -- attempt is clamped to [0,20]
	if d <= 0 || d > p.cap {
		d = p.cap
	}
	jitter := time.Duration(rand.Int64N(int64(d)/5 + 1)) // #nosec G404 -- jitter, not security
	return d + jitter
}

// shouldLog throttles retry logging to attempts 1, 2, 4, 8, ... so a stuck
// database yields a handful of lines per operation, not thousands per second.
func shouldLog(attempt int) bool {
	return attempt > 0 && attempt&(attempt-1) == 0
}

// retryTransient runs fn and retries transient errors with bounded, jittered
// exponential backoff. It returns the last error wrapped in ErrRetryExhausted
// when the attempt budget is spent.
func (o *ObservedDB) retryTransient(ctx context.Context, operation string, fn func() error) error {
	maxAttempts := o.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxAttempts()
	}
	err := fn()
	for attempt := 1; ; attempt++ {
		reason, ok := IsTransient(err)
		if !ok {
			return err
		}
		if attempt >= maxAttempts {
			dbRetriesExhausted.WithLabelValues(o.storeName, reason).Inc()
			slog.Error("db: transient error persisted, giving up",
				"store", o.storeName, "operation", operation, "reason", reason,
				"attempts", attempt, "error", err)
			return fmt.Errorf("%w after %d attempts: %w", ErrRetryExhausted, attempt, err)
		}
		backoff := Backoff(attempt-1, policyFor(reason))
		dbTransientRetries.WithLabelValues(o.storeName, reason).Inc()
		if reason == ReasonWSREP {
			dbWSREPRetries.Inc()
		}
		if shouldLog(attempt) {
			slog.Warn("db: transient error, retrying",
				"store", o.storeName, "operation", operation, "reason", reason,
				"attempt", attempt, "max_attempts", maxAttempts, "backoff", backoff)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %w", ctx.Err(), err)
		case <-t.C:
		}
		err = fn()
	}
}

// RetryShort runs fn up to three times with 50/100/200 ms delays when it
// returns a transient error. Intended for callers outside the store layer
// that already hold no transaction (bridge subscriber, claim helpers).
func RetryShort(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = fn()
		if err == nil || !IsRetryable(err) {
			return err
		}
		delay := 50 * time.Millisecond << uint(attempt) // #nosec G115 -- attempt < 3
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %w", ctx.Err(), err)
		case <-t.C:
		}
	}
	return err
}
