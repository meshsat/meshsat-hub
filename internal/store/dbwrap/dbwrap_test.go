package dbwrap

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestIsWSREPNotReady(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic", fmt.Errorf("connection refused"), false},
		{"mysql other", &mysql.MySQLError{Number: 1045, Message: "Access denied"}, false},
		{"wsrep 1047", &mysql.MySQLError{Number: 1047, Message: "WSREP has not yet prepared node for application use"}, true},
		{"wrapped 1047", fmt.Errorf("query failed: %w", &mysql.MySQLError{Number: 1047, Message: "WSREP"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWSREPNotReady(tt.err); got != tt.want {
				t.Errorf("isWSREPNotReady(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWSREPBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // cap
		{7, 60 * time.Second}, // stays at cap
		{20, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			if got := wsrepBackoff(tt.attempt); got != tt.want {
				t.Errorf("wsrepBackoff(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
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

func TestRetryOnWSREP_NoRetryOnOtherError(t *testing.T) {
	mock := &mockDB{
		execErrors: []error{fmt.Errorf("connection refused")},
	}
	obs := NewObservedDB(mock, "test", 0)
	_, err := obs.ExecContext(context.Background(), "INSERT INTO x VALUES (1)")
	if err == nil || err.Error() != "connection refused" {
		t.Errorf("expected 'connection refused', got %v", err)
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 call, got %d", mock.callCount)
	}
}

func TestRetryOnWSREP_RetriesAndSucceeds(t *testing.T) {
	wsrepErr := &mysql.MySQLError{Number: 1047, Message: "WSREP has not yet prepared node for application use"}
	mock := &mockDB{
		execErrors: []error{wsrepErr, wsrepErr, nil}, // fail twice, succeed on 3rd
	}
	obs := NewObservedDB(mock, "test", 0)
	_, err := obs.ExecContext(context.Background(), "INSERT INTO x VALUES (1)")
	if err != nil {
		t.Errorf("expected nil error after retry, got %v", err)
	}
	if mock.callCount != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", mock.callCount)
	}
}

func TestRetryOnWSREP_RespectsContextCancellation(t *testing.T) {
	wsrepErr := &mysql.MySQLError{Number: 1047, Message: "WSREP"}
	mock := &mockDB{
		execErrors: []error{wsrepErr, wsrepErr, wsrepErr, wsrepErr, wsrepErr},
	}
	obs := NewObservedDB(mock, "test", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_, err := obs.ExecContext(ctx, "INSERT INTO x VALUES (1)")
	// Should have retried once (1s backoff) then context expired during 2nd backoff (2s)
	if !isWSREPNotReady(err) {
		t.Errorf("expected WSREP error after context cancellation, got %v", err)
	}
	// At least 2 calls: initial + 1 retry (1s backoff fits in 1.5s window)
	if mock.callCount < 2 {
		t.Errorf("expected at least 2 calls, got %d", mock.callCount)
	}
}
