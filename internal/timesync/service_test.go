package timesync

import (
	"context"
	"testing"
	"time"
)

func TestTimeServiceBasic(t *testing.T) {
	ts := NewTimeService(nil)

	// Default stratum should be 5 (local RTC).
	if ts.Stratum() != 5 {
		t.Fatalf("expected stratum 5, got %d", ts.Stratum())
	}

	// Default offset should be 0.
	if ts.Offset() != 0 {
		t.Fatalf("expected offset 0, got %d", ts.Offset())
	}

	// Now() should be close to time.Now().
	diff := time.Until(ts.Now())
	if diff > 10*time.Millisecond || diff < -10*time.Millisecond {
		t.Fatalf("Now() differs from time.Now() by %v", diff)
	}
}

func TestTimeServiceApplyCorrection(t *testing.T) {
	ts := NewTimeService(nil)

	// Apply a stratum-1 correction with 500ms offset.
	offsetNs := int64(500_000_000)
	ts.ApplyCorrection("test_gps", 1, offsetNs, 100_000_000)

	if ts.Stratum() != 1 {
		t.Fatalf("expected stratum 1, got %d", ts.Stratum())
	}
	if ts.Offset() != offsetNs {
		t.Fatalf("expected offset %d, got %d", offsetNs, ts.Offset())
	}

	status := ts.GetStatus()
	if status.Source != "test_gps" {
		t.Fatalf("expected source test_gps, got %s", status.Source)
	}
}

func TestTimeServiceStratumPriority(t *testing.T) {
	ts := NewTimeService(nil)

	// Apply stratum 2.
	ts.ApplyCorrection("hub_ntp", 2, 100_000_000, 500_000_000)
	if ts.Stratum() != 2 {
		t.Fatalf("expected stratum 2, got %d", ts.Stratum())
	}

	// Apply stratum 1 -- should take precedence.
	ts.ApplyCorrection("gps", 1, 50_000_000, 100_000_000)
	if ts.Stratum() != 1 {
		t.Fatalf("expected stratum 1, got %d", ts.Stratum())
	}
	if ts.Offset() != 50_000_000 {
		t.Fatalf("expected offset 50_000_000, got %d", ts.Offset())
	}

	// Apply stratum 3 -- should be rejected (worse).
	ts.ApplyCorrection("mesh", 3, 200_000_000, 1_000_000_000)
	if ts.Stratum() != 1 {
		t.Fatalf("stratum 3 should not override stratum 1")
	}
	if ts.Offset() != 50_000_000 {
		t.Fatalf("offset should not change")
	}
}

func TestTimeServiceUncertaintyPriority(t *testing.T) {
	ts := NewTimeService(nil)

	// Apply stratum 1, uncertainty 200ms.
	ts.ApplyCorrection("source_a", 1, 100_000_000, 200_000_000)
	if ts.GetStatus().UncertaintyMs != 200.0 {
		t.Fatal("uncertainty mismatch")
	}

	// Apply stratum 1, uncertainty 100ms -- should win (lower uncertainty).
	ts.ApplyCorrection("source_b", 1, 80_000_000, 100_000_000)
	if ts.GetStatus().Source != "source_b" {
		t.Fatal("lower uncertainty source should take precedence")
	}

	// Apply stratum 1, uncertainty 300ms -- should be rejected (higher).
	ts.ApplyCorrection("source_c", 1, 120_000_000, 300_000_000)
	if ts.GetStatus().Source != "source_b" {
		t.Fatal("higher uncertainty should not override")
	}
}

func TestTimeServiceSetPeerCount(t *testing.T) {
	ts := NewTimeService(nil)

	ts.SetPeerCount(5)
	status := ts.GetStatus()
	if status.Peers != 5 {
		t.Fatalf("expected 5 peers, got %d", status.Peers)
	}
}

func TestTimeServiceAddSourceAndStart(t *testing.T) {
	ts := NewTimeService(nil)

	started := make(chan bool, 1)
	ts.AddSource(&testSource{
		name:      "test",
		stratum:   2,
		onStarted: started,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts.Start(ctx)

	select {
	case <-started:
		// Source was started.
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for source to start")
	}
}

func TestTimeServiceUnixNano(t *testing.T) {
	ts := NewTimeService(nil)
	ts.ApplyCorrection("test", 1, 1_000_000_000, 0) // +1s offset

	unixNano := ts.UnixNano()
	expected := time.Now().UnixNano() + 1_000_000_000
	diff := unixNano - expected
	if diff > 10_000_000 || diff < -10_000_000 { // 10ms tolerance
		t.Fatalf("UnixNano diff too large: %d ns", diff)
	}
}

// testSource is a minimal TimeSource for testing.
type testSource struct {
	name      string
	stratum   int
	onStarted chan bool
}

func (s *testSource) Name() string { return s.name }
func (s *testSource) Stratum() int { return s.stratum }
func (s *testSource) Start(ctx context.Context, cb CorrectionCallback) {
	if s.onStarted != nil {
		s.onStarted <- true
	}
	<-ctx.Done()
}

// TestTimeSyncStore implementation for testing.
type mockStore struct {
	source        string
	stratum       int
	offsetNs      int64
	uncertaintyNs int64
	hasSaved      bool
}

func (m *mockStore) LoadBestOffset() (string, int, int64, int64, bool) {
	if m.source == "" {
		return "", 0, 0, 0, false
	}
	return m.source, m.stratum, m.offsetNs, m.uncertaintyNs, true
}

func (m *mockStore) SaveOffset(source string, stratum int, offsetNs, uncertaintyNs int64, peerHash string) {
	m.source = source
	m.stratum = stratum
	m.offsetNs = offsetNs
	m.uncertaintyNs = uncertaintyNs
	m.hasSaved = true
}

func TestTimeServicePersistence(t *testing.T) {
	store := &mockStore{}
	ts := NewTimeService(store)

	// Apply a correction -- should be saved.
	ts.ApplyCorrection("gps", 1, 42_000_000, 10_000_000)
	if !store.hasSaved {
		t.Fatal("expected store to receive save")
	}
	if store.source != "gps" || store.offsetNs != 42_000_000 {
		t.Fatal("store values mismatch")
	}

	// Create a new service and restore.
	ts2 := NewTimeService(store)
	ts2.LoadPersistedState()
	if ts2.Offset() != 42_000_000 {
		t.Fatalf("expected restored offset 42_000_000, got %d", ts2.Offset())
	}
	if ts2.Stratum() != 1 {
		t.Fatalf("expected restored stratum 1, got %d", ts2.Stratum())
	}
}

func TestTimeServiceNilStore(t *testing.T) {
	ts := NewTimeService(nil)

	// Should not panic.
	ts.LoadPersistedState()
	ts.ApplyCorrection("test", 1, 0, 0)
}
