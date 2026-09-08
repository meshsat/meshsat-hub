package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

type countingSender struct{ n atomic.Int32 }

func (c *countingSender) SendScheduled(_ context.Context, _ *store.Message) error {
	c.n.Add(1)
	return nil
}

// TestTwoSchedulersSendOnce: two replicas polling the same store send a due
// scheduled message exactly once.
func TestTwoSchedulersSendOnce(t *testing.T) {
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: "300234063904190", Label: "T", Type: "rockblock"})
	m := &store.Message{DeviceIMEI: "300234063904190", Direction: "mt", Channel: "iridium", Text: "later", Status: "scheduled", ScheduledAt: time.Now().Add(-time.Minute)}
	if err := s.InsertMessage(ctx, store.DefaultTenantID, m); err != nil {
		t.Fatal(err)
	}
	sender := &countingSender{}
	a := New(s, sender, time.Minute)
	b := New(s, sender, time.Minute)
	a.tick(ctx)
	b.tick(ctx)
	a.tick(ctx)
	if sender.n.Load() != 1 {
		t.Fatalf("expected exactly 1 send, got %d", sender.n.Load())
	}
	got, _ := s.GetMessage(ctx, store.DefaultTenantID, m.ID)
	if got.Status != "sent" {
		t.Errorf("status after send: %q", got.Status)
	}
}
