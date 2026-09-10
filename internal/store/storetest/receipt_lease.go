package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testReceiptLease pins the per-row claim on the receipts outbox against a real
// database in both dialects.
//
// A receipt draws an invoice number out of a gapless legal series. Two drainers
// working one row create two documents for one payment and leave the first
// permanently unpaid in the books, and that is not something a retry undoes.
// Until this lease existed only timing prevented it -- one leader, a stop wait
// longer than the billing call, a pod grace period longer than the stop wait --
// so correctness rested on those margins holding (MESHSAT-998).
func testReceiptLease(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC()

	r := &store.Receipt{
		ID: "rcp-lease", TenantID: "default", DeliveryKey: "lease-key-1",
		Email: "payer@example.com", AmountCents: 900, Currency: "EUR",
		Plan: "crew", Status: store.ReceiptPending, PaidAt: now, NextAttemptAt: now.Add(-time.Minute),
	}
	if created, err := s.CreateReceipt(ctx, r); err != nil || !created {
		t.Fatalf("create receipt: created=%v err=%v", created, err)
	}

	// Ten drainers reach for the same row at once. Exactly one may have it.
	const racers = 10
	won := make([]bool, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			won[i], errs[i] = s.ClaimReceipt(ctx, r.ID, time.Now().UTC().Add(5*time.Minute))
		}(i)
	}
	close(start)
	wg.Wait()

	n := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if won[i] {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d drainers claimed one receipt; exactly one may draw its invoice number", n)
	}

	// While it is held, the row must not come back as due -- a second drainer
	// should never even see it.
	due, err := s.ListDueReceipts(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == r.ID {
			t.Error("a leased receipt is still listed as due; another drainer would pick it up")
		}
	}

	// Releasing puts it back in the queue immediately, so a receipt that failed
	// and is due again in a minute is not held for the rest of its lease.
	if err := s.ReleaseReceipt(ctx, r.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	due, err = s.ListDueReceipts(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.ID == r.ID {
			found = true
		}
	}
	if !found {
		t.Error("a released receipt did not come back as due")
	}

	// An expired lease is reclaimable without anyone releasing it: that is what
	// makes a drainer killed mid-issue recoverable rather than a stuck row.
	if ok, err := s.ClaimReceipt(ctx, r.ID, time.Now().UTC().Add(-time.Second)); err != nil || !ok {
		t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimReceipt(ctx, r.ID, time.Now().UTC().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("an expired lease was not reclaimable: ok=%v err=%v", ok, err)
	}
}
