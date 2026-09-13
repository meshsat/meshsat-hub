// Package bridge implements the Hub-side MQTT subscriber for bridge lifecycle events.
package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Reaper periodically marks bridges as offline when their last_seen exceeds
// a configurable timeout. This handles the common case where a bridge
// disappears without sending a death message (network loss, power cut, crash).
type Reaper struct {
	store    store.Store
	timeout  time.Duration
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewReaper creates a reaper that marks bridges offline after timeout of
// inactivity, where timeout is the PLATFORM DEFAULT. A tenant that has chosen
// its own value overrides it in the store's query; this constructor only
// decides how often to look.
//
// NewReaperWithFloor is what main.go uses, because a tenant may legitimately
// choose a shorter timeout than the platform default and the tick has to be
// fast enough for the shortest one in play.
func NewReaper(s store.Store, timeout time.Duration) *Reaper {
	return NewReaperWithFloor(s, timeout, timeout)
}

// NewReaperWithFloor is NewReaper where floor is the SHORTEST timeout any
// tenant may have (the platform minimum). The check interval is half of
// whichever is smaller, so a tenant on a one-minute timeout is not reaped on
// the five-minute default's schedule -- which would have made its choice a
// number that quietly did nothing for most of its range (MESHSAT-1117).
func NewReaperWithFloor(s store.Store, timeout, floor time.Duration) *Reaper {
	shortest := timeout
	if floor > 0 && floor < shortest {
		shortest = floor
	}
	interval := shortest / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	return &Reaper{
		store:    s,
		timeout:  timeout,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the periodic reaper loop in a background goroutine.
func (r *Reaper) Start() {
	slog.Info("bridge: reaper started",
		"timeout", r.timeout.String(),
		"interval", r.interval.String(),
	)
	go r.loop()
}

// Stop signals the reaper to stop and waits for the goroutine to exit.
func (r *Reaper) Stop() {
	close(r.stop)
	<-r.done
	slog.Info("bridge: reaper stopped")
}

func (r *Reaper) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.reap()
		}
	}
}

func (r *Reaper) reap() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, err := r.store.MarkStaleBridgesOffline(ctx, r.timeout)
	if err != nil {
		slog.Error("bridge: reaper failed", "error", err)
		return
	}
	if n > 0 {
		slog.Warn("bridge: reaper marked bridges offline", "count", n, "timeout", r.timeout.String())
	}
}
