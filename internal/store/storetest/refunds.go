package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testRefunds pins the refunds outbox against a real database in both dialects
// (MESHSAT-1019).
//
// Three things here are money guards rather than plumbing, and none of them can
// be proven with an in-memory fake:
//
//   - receipt_id is UNIQUE, so a payment is refunded once. A second record
//     against the same payment would credit money that only went back once.
//   - the per-row lease is decided by the database, so two drainers cannot both
//     draw a credit note number out of the same gapless series.
//   - a refund that has produced a credit note cannot be deleted, because
//     deleting the row would hide the document rather than unmake it.
func testRefunds(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC()

	// A refund reverses a payment, so the payment has to exist first.
	receipt := &store.Receipt{
		TenantID: "t-refund", DeliveryKey: "refund-key", Email: "buyer@example.com",
		AmountCents: 900, Currency: "EUR", Country: "NL", Plan: "crew",
		Status: store.ReceiptPending, PaidAt: now,
	}
	if created, err := s.CreateReceipt(ctx, receipt); err != nil || !created {
		t.Fatalf("seeding the payment: created=%v err=%v", created, err)
	}
	got, err := s.GetReceipt(ctx, receipt.ID)
	if err != nil || got == nil || got.AmountCents != 900 {
		t.Fatalf("GetReceipt: %+v err=%v", got, err)
	}
	if _, err := s.GetReceipt(ctx, "no-such-receipt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetReceipt of a missing payment: want ErrNotFound, got %v", err)
	}

	r := &store.Refund{
		TenantID: "t-refund", ReceiptID: receipt.ID, AmountCents: 900, Currency: "EUR",
		Country: "NL", Reason: "14-day withdrawal", RequestedBy: "admin@meshsat.net",
		RefundedAt: now, NextAttemptAt: now.Add(-time.Minute),
	}
	created, err := s.CreateRefund(ctx, r)
	if err != nil || !created {
		t.Fatalf("CreateRefund: created=%v err=%v", created, err)
	}

	// One refund per payment. The constraint is the check, not a read.
	second := &store.Refund{
		TenantID: "t-refund", ReceiptID: receipt.ID, AmountCents: 900, Currency: "EUR",
		RefundedAt: now,
	}
	if created, err := s.CreateRefund(ctx, second); err != nil {
		t.Fatalf("second CreateRefund errored instead of reporting a duplicate: %v", err)
	} else if created {
		t.Fatal("a payment was refunded twice; receipt_id is not unique")
	}

	if fetched, err := s.GetRefundByReceipt(ctx, receipt.ID); err != nil || fetched == nil {
		t.Fatalf("GetRefundByReceipt: %+v err=%v", fetched, err)
	} else if fetched.ID != r.ID || fetched.Reason != "14-day withdrawal" ||
		fetched.RequestedBy != "admin@meshsat.net" {
		t.Fatalf("GetRefundByReceipt returned the wrong row or lost fields: %+v", fetched)
	}

	// The lease is the database's decision. Two drainers, one winner.
	var wg sync.WaitGroup
	won := make([]bool, 8)
	errs := make([]error, 8)
	for i := range won {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			won[i], errs[i] = s.ClaimRefund(ctx, r.ID, time.Now().UTC().Add(10*time.Minute))
		}(i)
	}
	wg.Wait()
	winners := 0
	for i := range won {
		if errs[i] != nil {
			t.Fatalf("ClaimRefund errored: %v", errs[i])
		}
		if won[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("ClaimRefund had %d winners, want exactly 1; two drainers would draw two credit note numbers", winners)
	}

	// A leased row is invisible to the drainer until the lease expires.
	due, err := s.ListDueRefunds(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("ListDueRefunds: %v", err)
	}
	for _, d := range due {
		if d.ID == r.ID {
			t.Fatal("a leased refund was listed as due")
		}
	}
	if err := s.ReleaseRefund(ctx, r.ID); err != nil {
		t.Fatalf("ReleaseRefund: %v", err)
	}
	due, err = s.ListDueRefunds(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("ListDueRefunds after release: %v", err)
	}
	found := false
	for _, d := range due {
		if d.ID == r.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a released refund is not due again")
	}

	// The credit note id is persisted the instant the document exists, before
	// it is numbered. That is what stops a retry creating a second one.
	if err := s.SetRefundCredit(ctx, r.ID, "credit-abc"); err != nil {
		t.Fatalf("SetRefundCredit: %v", err)
	}
	// And once it exists, the row cannot be deleted: deleting it would hide the
	// document, not unmake it.
	if err := s.DeleteRefund(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refund with a credit note was deletable (err=%v)", err)
	}

	if err := s.MarkRefundIssued(ctx, r.ID, "MSHCN2026-0001", "credit-abc", now); err != nil {
		t.Fatalf("MarkRefundIssued: %v", err)
	}
	issued, err := s.ListRefundsByStatus(ctx, store.RefundIssued, 10)
	if err != nil || len(issued) != 1 {
		t.Fatalf("ListRefundsByStatus(issued): %d rows err=%v", len(issued), err)
	}
	if issued[0].CreditNumber != "MSHCN2026-0001" || issued[0].IssuedAt == nil {
		t.Fatalf("issued refund lost its document: %+v", issued[0])
	}

	// An issued refund never goes back to pending: the drainer would draw a
	// second number for money that already has a document.
	if err := s.RequeueRefund(ctx, r.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an issued refund was requeued (err=%v); that is a second credit note", err)
	}

	// The threshold meter subtracts refunds: a sale given back is not a supply.
	by, err := s.RefundsByCountrySince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("RefundsByCountrySince: %v", err)
	}
	if by["NL"] != 900 {
		t.Fatalf("RefundsByCountrySince = %v, want NL=900", by)
	}

	// A blocked refund can be requeued, and a pending one with no document can
	// be withdrawn so a wrong figure can be corrected.
	r2 := &store.Refund{
		TenantID: "t-refund", ReceiptID: "receipt-2", AmountCents: 500, Currency: "EUR",
		Country: "DE", RefundedAt: now, NextAttemptAt: now,
	}
	if created, err := s.CreateRefund(ctx, r2); err != nil || !created {
		t.Fatalf("CreateRefund r2: created=%v err=%v", created, err)
	}
	if err := s.BlockRefund(ctx, r2.ID, "the invoice is gone"); err != nil {
		t.Fatalf("BlockRefund: %v", err)
	}
	blocked, err := s.ListRefundsByStatus(ctx, store.RefundBlocked, 10)
	if err != nil || len(blocked) != 1 || blocked[0].LastError != "the invoice is gone" {
		t.Fatalf("ListRefundsByStatus(blocked): %+v err=%v", blocked, err)
	}
	if err := s.RequeueRefund(ctx, r2.ID, now); err != nil {
		t.Fatalf("RequeueRefund of a blocked refund: %v", err)
	}
	if err := s.MarkRefundAttempt(ctx, r2.ID, "billing system down", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkRefundAttempt: %v", err)
	}
	if err := s.DeleteRefund(ctx, r2.ID); err != nil {
		t.Fatalf("DeleteRefund of an uncredited refund: %v", err)
	}
	if _, err := s.GetRefund(ctx, r2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a withdrawn refund is still there (err=%v)", err)
	}
}
