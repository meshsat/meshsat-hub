package leader

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// flipLeader is a Leader whose acquire/lose transitions the test drives.
type flipLeader struct {
	acquire chan struct{}
	lose    chan struct{}
}

func (f *flipLeader) Run(ctx context.Context, onAcquired func(), onLost func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.acquire:
			onAcquired()
		case <-f.lose:
			onLost()
		}
	}
}
func (f *flipLeader) IsLeader() bool { return false }

func TestSingletonsStartOnAcquireStopOnLose(t *testing.T) {
	var running atomic.Int32
	var starts atomic.Int32
	s := NewSingletons(Singleton{Name: "svc", Run: func(ctx context.Context) {
		starts.Add(1)
		running.Add(1)
		<-ctx.Done()
		running.Add(-1)
	}})
	fl := &flipLeader{acquire: make(chan struct{}), lose: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunWith(ctx, fl, s, nil, nil)

	fl.acquire <- struct{}{}
	waitFor(t, func() bool { return running.Load() == 1 })
	if !s.Running() {
		t.Fatal("Running should be true after acquisition")
	}
	fl.acquire <- struct{}{} // duplicate acquisition is a no-op
	time.Sleep(20 * time.Millisecond)
	if starts.Load() != 1 {
		t.Fatalf("service started %d times, want 1", starts.Load())
	}
	fl.lose <- struct{}{}
	waitFor(t, func() bool { return running.Load() == 0 })
	if s.Running() {
		t.Fatal("Running should be false after loss")
	}
	fl.acquire <- struct{}{} // re-acquisition restarts the service
	waitFor(t, func() bool { return starts.Load() == 2 && running.Load() == 1 })
}

func TestSingletonsPanicIsContained(t *testing.T) {
	var ok atomic.Int32
	s := NewSingletons(
		Singleton{Name: "bad", Run: func(context.Context) { panic("boom") }},
		Singleton{Name: "good", Run: func(ctx context.Context) { ok.Add(1); <-ctx.Done() }},
	)
	s.Start(context.Background())
	waitFor(t, func() bool { return ok.Load() == 1 })
	s.Stop()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// A departing leader must outwait the longest call one of its singletons can
// be inside, or the handover overlaps with work in flight. The receipt issuer
// is the one that matters: it talks to the billing system with a 30 s timeout,
// and two owners drawing invoice numbers from one gapless series at the same
// time is not something a retry can undo (MESHSAT-998).
func TestStopWaitOutlivesTheLongestOutboundCall(t *testing.T) {
	const billingTimeout = 30 * time.Second
	if DefaultStopWait <= billingTimeout {
		t.Fatalf("DefaultStopWait is %s, which does not outlive a %s billing call; "+
			"raise it or a leader handover mid-invoice runs two owners at once",
			DefaultStopWait, billingTimeout)
	}
}
