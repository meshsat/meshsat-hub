package timesync

import (
	"context"
	"testing"
	"time"
)

func TestLocalNTPSourceStartsAndReportsZeroOffset(t *testing.T) {
	src := NewLocalNTPSource()

	if src.Name() != "local_ntp" {
		t.Fatalf("expected name local_ntp, got %s", src.Name())
	}
	if src.Stratum() != 1 {
		t.Fatalf("expected stratum 1, got %d", src.Stratum())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan struct{}, 1)
	var gotSource string
	var gotStratum int
	var gotOffsetNs int64

	cb := func(source string, stratum int, offsetNs, uncertaintyNs int64) {
		gotSource = source
		gotStratum = stratum
		gotOffsetNs = offsetNs
		select {
		case received <- struct{}{}:
		default:
		}
	}

	go src.Start(ctx, cb)

	select {
	case <-received:
		if gotSource != "local_ntp" {
			t.Fatalf("expected source local_ntp, got %s", gotSource)
		}
		if gotStratum != 1 {
			t.Fatalf("expected stratum 1, got %d", gotStratum)
		}
		if gotOffsetNs != 0 {
			t.Fatalf("expected offset 0, got %d", gotOffsetNs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial callback")
	}
}

func TestHubNTPSourceInjectReading(t *testing.T) {
	src := NewHubNTPSource()

	if src.Name() != "hub_ntp" {
		t.Fatalf("expected name hub_ntp, got %s", src.Name())
	}
	if src.Stratum() != 2 {
		t.Fatalf("expected stratum 2, got %d", src.Stratum())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan struct{}, 1)
	var gotSource string

	cb := func(source string, stratum int, offsetNs, uncertaintyNs int64) {
		gotSource = source
		select {
		case received <- struct{}{}:
		default:
		}
	}

	go src.Start(ctx, cb)

	// Inject a reading (simulate bridge time).
	src.InjectReading(time.Now().UnixNano(), 1)

	select {
	case <-received:
		if gotSource != "hub_ntp" {
			t.Fatalf("expected source hub_ntp, got %s", gotSource)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for injected reading callback")
	}
}

func TestHubNTPSourceDropsWhenFull(t *testing.T) {
	src := NewHubNTPSource()

	// Fill the buffer (capacity 8).
	for i := 0; i < 10; i++ {
		src.InjectReading(time.Now().UnixNano(), 1)
	}

	// Should not block or panic -- drops are silent.
}

func TestLocalNTPSourceCancellation(t *testing.T) {
	src := NewLocalNTPSource()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		src.Start(ctx, func(string, int, int64, int64) {})
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("source did not exit after context cancellation")
	}
}
