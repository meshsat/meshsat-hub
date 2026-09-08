package dbwrap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"
)

// pgErr mimics pgconn.PgError / pq.Error without importing a Postgres driver.
type pgErr struct{ code string }

func (e *pgErr) Error() string    { return "pg: " + e.code }
func (e *pgErr) SQLState() string { return e.code }

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		reason string
		ok     bool
	}{
		{"nil", nil, "", false},
		{"generic", fmt.Errorf("connection refused"), "", false},
		{"bad conn", driver.ErrBadConn, ReasonConnection, true},
		{"wrapped bad conn", fmt.Errorf("exec: %w", driver.ErrBadConn), ReasonConnection, true},
		{"pg serialization", &pgErr{"40001"}, ReasonSerialize, true},
		{"pg deadlock", &pgErr{"40P01"}, ReasonDeadlock, true},
		{"wrapped pg deadlock", fmt.Errorf("q: %w", &pgErr{"40P01"}), ReasonDeadlock, true},
		{"pg admin shutdown", &pgErr{"57P01"}, ReasonPGShutdown, true},
		{"pg crash shutdown", &pgErr{"57P02"}, ReasonPGShutdown, true},
		{"pg cannot connect now", &pgErr{"57P03"}, ReasonPGStarting, true},
		{"pg read only", &pgErr{"25006"}, ReasonPGReadOnly, true},
		{"pg connection class", &pgErr{"08006"}, ReasonConnection, true},
		{"pg unique violation", &pgErr{"23505"}, "", false},
		{"pg syntax", &pgErr{"42601"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := IsTransient(tt.err)
			if ok != tt.ok || reason != tt.reason {
				t.Errorf("IsTransient(%v) = (%q,%v), want (%q,%v)", tt.err, reason, ok, tt.reason, tt.ok)
			}
			if IsRetryable(tt.err) != tt.ok {
				t.Errorf("IsRetryable mismatch for %v", tt.err)
			}
		})
	}
}

// TestBackoffNeverNegative is the regression test for Incident 20a: the old
// math.Pow(2, attempt) overflowed int64 at attempt 63, the cap never fired and
// the timer fired instantly (backoff:0) at ~1,000 retries/s for five days.
func TestBackoffNeverNegative(t *testing.T) {
	for _, p := range []policy{policyFor(ReasonPGStarting), policyFor(ReasonDeadlock)} {
		for attempt := -1; attempt <= 10_000; attempt++ {
			d := Backoff(attempt, p)
			if d <= 0 {
				t.Fatalf("Backoff(%d) = %v, must be positive", attempt, d)
			}
			if d > p.cap+p.cap/5 {
				t.Fatalf("Backoff(%d) = %v exceeds cap %v + 20%% jitter", attempt, d, p.cap)
			}
		}
	}
}

func TestBackoffGrowsThenCaps(t *testing.T) {
	p := policyFor(ReasonPGStarting) // slow policy
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for attempt, base := range want {
		got := Backoff(attempt, p)
		if got < base || got > base+base/5 {
			t.Errorf("Backoff(%d) = %v, want %v..%v", attempt, got, base, base+base/5)
		}
	}
	fast := policyFor(ReasonDeadlock)
	if d := Backoff(0, fast); d < 50*time.Millisecond || d > 60*time.Millisecond {
		t.Errorf("deadlock first backoff = %v, want ~50ms", d)
	}
}

func TestShouldLogThrottles(t *testing.T) {
	logged := 0
	for attempt := 1; attempt <= 1024; attempt++ {
		if shouldLog(attempt) {
			logged++
		}
	}
	if logged != 11 { // 1,2,4,...,1024
		t.Errorf("shouldLog fired %d times over 1024 attempts, want 11", logged)
	}
}

// mockDB is a minimal SQLDB implementation for testing retries.
type mockDB struct {
	execErrors []error // errors to return on successive ExecContext calls
	callCount  int
}

func (m *mockDB) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	idx := m.callCount
	m.callCount++
	if idx < len(m.execErrors) {
		return nil, m.execErrors[idx]
	}
	return nil, nil
}

func (m *mockDB) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, nil
}

func (m *mockDB) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

func (m *mockDB) PingContext(_ context.Context) error { return nil }
func (m *mockDB) Close() error                        { return nil }
func (m *mockDB) Stats() sql.DBStats                  { return sql.DBStats{} }

func TestRetry_NoRetryOnOtherError(t *testing.T) {
	mock := &mockDB{execErrors: []error{fmt.Errorf("connection refused")}}
	obs := NewObservedDB(mock, "test", 0)
	_, err := obs.ExecContext(context.Background(), "INSERT INTO x VALUES (1)")
	if err == nil || err.Error() != "connection refused" {
		t.Errorf("expected 'connection refused', got %v", err)
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 call, got %d", mock.callCount)
	}
}

func TestRetry_RetriesAndSucceeds(t *testing.T) {
	deadlock := &pgErr{"40P01"} // fast policy keeps the test quick
	mock := &mockDB{execErrors: []error{deadlock, deadlock, nil}}
	obs := NewObservedDB(mock, "test", 0)
	_, err := obs.ExecContext(context.Background(), "INSERT INTO x VALUES (1)")
	if err != nil {
		t.Errorf("expected nil error after retry, got %v", err)
	}
	if mock.callCount != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", mock.callCount)
	}
}

func TestRetry_Bounded(t *testing.T) {
	deadlock := &pgErr{"40P01"}
	errs := make([]error, 50)
	for i := range errs {
		errs[i] = deadlock
	}
	mock := &mockDB{execErrors: errs}
	obs := NewObservedDB(mock, "test", 0)
	obs.MaxAttempts = 4
	_, err := obs.ExecContext(context.Background(), "INSERT INTO x VALUES (1)")
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("expected ErrRetryExhausted, got %v", err)
	}
	var pe *pgErr
	if !errors.As(err, &pe) || pe.SQLState() != "40P01" {
		t.Errorf("exhausted error should wrap the last cause, got %v", err)
	}
	if mock.callCount != 4 {
		t.Errorf("expected exactly MaxAttempts=4 calls, got %d", mock.callCount)
	}
}

func TestRetry_RespectsContextCancellation(t *testing.T) {
	starting := &pgErr{"57P03"} // slow policy: 1 s first backoff
	mock := &mockDB{execErrors: []error{starting, starting, starting, starting, starting}}
	obs := NewObservedDB(mock, "test", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := obs.ExecContext(ctx, "INSERT INTO x VALUES (1)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context deadline error, got %v", err)
	}
	if !IsRetryable(err) {
		t.Errorf("cancelled retry should still expose the transient cause, got %v", err)
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 call before the 1s backoff was cut short, got %d", mock.callCount)
	}
}

func TestSetDefaultMaxAttempts(t *testing.T) {
	orig := DefaultMaxAttempts()
	t.Cleanup(func() { SetDefaultMaxAttempts(orig) })
	SetDefaultMaxAttempts(0)
	if DefaultMaxAttempts() != orig {
		t.Errorf("zero must be ignored")
	}
	SetDefaultMaxAttempts(3)
	if got := NewObservedDB(&mockDB{}, "t", 0).MaxAttempts; got != 3 {
		t.Errorf("new ObservedDB MaxAttempts = %d, want 3", got)
	}
}

func TestRetryShort(t *testing.T) {
	calls := 0
	err := RetryShort(context.Background(), func() error {
		calls++
		if calls < 3 {
			return &pgErr{"40P01"}
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("RetryShort: err=%v calls=%d, want nil/3", err, calls)
	}
	calls = 0
	err = RetryShort(context.Background(), func() error {
		calls++
		return fmt.Errorf("permanent")
	})
	if err == nil || calls != 1 {
		t.Errorf("RetryShort must not retry permanent errors: err=%v calls=%d", err, calls)
	}
}
