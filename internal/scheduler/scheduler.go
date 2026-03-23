// Package scheduler implements a background scheduler for delivering
// messages that were queued for future delivery (scheduled_at).
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// MessageSender sends a scheduled message via the appropriate transport.
type MessageSender interface {
	SendScheduled(ctx context.Context, msg *store.Message) error
}

// Scheduler polls the store for due scheduled messages and dispatches them.
type Scheduler struct {
	store    store.Store
	sender   MessageSender
	interval time.Duration
}

// New creates a Scheduler that ticks at the given interval.
// A zero or negative interval defaults to 30 seconds.
func New(s store.Store, sender MessageSender, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Scheduler{
		store:    s,
		sender:   sender,
		interval: interval,
	}
}

// Run polls for due scheduled messages until ctx is cancelled.
// It logs errors but never crashes — the caller owns the goroutine lifecycle.
func (s *Scheduler) Run(ctx context.Context) {
	slog.Info("scheduler: started", "interval", s.interval)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler: stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	msgs, err := s.store.ListScheduledMessages(ctx, time.Now(), 10)
	if err != nil {
		slog.Error("scheduler: list scheduled messages", "error", err)
		return
	}
	for _, msg := range msgs {
		m := msg // capture loop var
		sendErr := s.sender.SendScheduled(ctx, &m)
		status := "sent"
		errMsg := ""
		if sendErr != nil {
			status = "failed"
			errMsg = sendErr.Error()
			slog.Error("scheduler: send failed", "id", m.ID, "imei", m.DeviceIMEI, "error", sendErr)
		} else {
			slog.Info("scheduler: sent", "id", m.ID, "imei", m.DeviceIMEI)
		}
		if updateErr := s.store.UpdateMessageStatus(ctx, "", m.ID, status, errMsg); updateErr != nil {
			slog.Error("scheduler: update status failed", "id", m.ID, "error", updateErr)
		}
	}
}
