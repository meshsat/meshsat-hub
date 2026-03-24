// Package bridge implements the Hub-side MQTT subscriber for bridge lifecycle events.
package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
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

// NewReaper creates a reaper that marks bridges offline after timeout seconds
// of inactivity. It checks every interval (typically timeout/2).
func NewReaper(s store.Store, timeout time.Duration) *Reaper {
	interval := timeout / 2
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
