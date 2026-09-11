package billing

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/store"
	dto "github.com/prometheus/client_model/go"
)

func gauge(t *testing.T, g interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("reading the gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

// A receipt is a document owed for money already taken, so the drainer never
// abandons one: a failure it cannot get past retries quietly, at an hour's
// interval, forever. That is the right behaviour and it used to be completely
// silent -- a customer had paid, had no VAT document, and nothing said so.
func TestTheReceiptBacklogIsVisible(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	rc := &memReceipts{leases: map[string]time.Time{}}
	rc.rows = []*store.Receipt{
		{ID: "r1", Status: store.ReceiptPending, CreatedAt: now.Add(-9 * time.Hour)},
		{ID: "r2", Status: store.ReceiptPending, CreatedAt: now.Add(-20 * time.Minute)},
		{ID: "r3", Status: store.ReceiptIssued, CreatedAt: now.Add(-72 * time.Hour)},
		{ID: "r4", Status: store.ReceiptBlocked, CreatedAt: now.Add(-30 * time.Hour)},
	}
	j := &ReceiptJob{store: rc, now: func() time.Time { return now }}
	j.observeBacklog(context.Background(), now)

	if got := gauge(t, metrics.ReceiptsPending); got != 2 {
		t.Errorf("pending receipts = %v, want 2", got)
	}
	if got := gauge(t, metrics.ReceiptsBlocked); got != 1 {
		t.Errorf("blocked receipts = %v, want 1 (money taken, no document, waiting for a person)", got)
	}
	// The OLDEST, not the newest: a nine-hour-old receipt is the one that means
	// somebody paid this morning and still has no document.
	if got := gauge(t, metrics.ReceiptOldestPendingAge); got != 9*3600 {
		t.Errorf("oldest pending age = %vs, want %vs", got, 9*3600)
	}
	// An issued receipt is finished business and must not count as a backlog,
	// however old it is -- r3 is three days old.
}

// Measuring the backlog must never be able to stop a document being issued.
func TestABrokenBacklogQueryDoesNotStopTheDrainer(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	rc := &memReceipts{leases: map[string]time.Time{}, fail: true}
	j := &ReceiptJob{store: rc, now: func() time.Time { return now }, batch: 10}
	// Must not panic and must return: Once() calls observeBacklog first.
	j.Once(context.Background())
}
