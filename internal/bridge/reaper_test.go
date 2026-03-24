package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// mockReaperStore implements only the method the reaper needs.
type mockReaperStore struct {
	store.Store
	mu    sync.Mutex
	calls int
	ret   int64
}

func (m *mockReaperStore) MarkStaleBridgesOffline(_ context.Context, _ time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.ret, nil
}

func (m *mockReaperStore) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestReaperStartStop(t *testing.T) {
	ms := &mockReaperStore{}
	r := NewReaper(ms, 60*time.Second)
	r.Start()
	r.Stop() // should not hang
}

func TestReaperCallsStore(t *testing.T) {
	ms := &mockReaperStore{ret: 1}
	r := &Reaper{
		store:    ms,
		timeout:  5 * time.Minute,
		interval: 50 * time.Millisecond,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	r.Start()

	// Wait for at least one tick
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	if ms.getCalls() == 0 {
		t.Fatal("reaper never called MarkStaleBridgesOffline")
	}
}

func TestReaperMinInterval(t *testing.T) {
	ms := &mockReaperStore{}
	// Very short timeout — interval should clamp to 10s minimum
	r := NewReaper(ms, 5*time.Second)
	if r.interval < 10*time.Second {
		t.Fatalf("interval %v < 10s minimum", r.interval)
	}
}
