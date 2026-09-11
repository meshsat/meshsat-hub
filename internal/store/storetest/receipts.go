package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testReceipts pins the contract the receipt outbox depends on (MESHSAT-998).
//
// The three properties that matter are all about not losing or duplicating a
// document for money that was taken:
//
//   - the delivery key is unique, and a second insert reports false rather
//     than erroring or overwriting -- that is what makes a replayed webhook
//     produce no second invoice;
//   - a failed attempt leaves the row pending and pushes its next attempt out,
//     so nothing drops a receipt because a billing system was down;
//   - only rows that are pending AND due come back from ListDueReceipts.
func testReceipts(t *testing.T, db store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	r := &store.Receipt{
		TenantID:      "t-receipts",
		DeliveryKey:   "kofi-msg-1",
		TransactionID: "txn-1",
		Email:         "payer@example.org",
		Name:          "Payer Example",
		AmountCents:   900,
		Currency:      "EUR",
		Plan:          "crew",
		TierName:      "Crew Membership",
		PaidAt:        now,
	}
	created, err := db.CreateReceipt(ctx, r)
	if err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}
	if !created {
		t.Fatal("first CreateReceipt reported the row already existed")
	}
	if r.ID == "" {
		t.Fatal("CreateReceipt did not assign an id")
	}

	// A replayed delivery. Same key, different everything else: the row must
	// not be inserted and must not be overwritten either.
	dup := &store.Receipt{
		TenantID:    "someone-else",
		DeliveryKey: "kofi-msg-1",
		Email:       "attacker@example.invalid",
		AmountCents: 999999,
		Currency:    "EUR",
		PaidAt:      now,
	}
	created, err = db.CreateReceipt(ctx, dup)
	if err != nil {
		t.Fatalf("duplicate CreateReceipt errored instead of reporting false: %v", err)
	}
	if created {
		t.Fatal("duplicate delivery key was inserted; a replayed payment would produce a second invoice")
	}

	got, err := db.GetReceiptByKey(ctx, "kofi-msg-1")
	if err != nil {
		t.Fatalf("GetReceiptByKey: %v", err)
	}
	if got.TenantID != "t-receipts" || got.AmountCents != 900 || got.Email != "payer@example.org" {
		t.Fatalf("the duplicate overwrote the original row: %+v", got)
	}
	if got.Status != store.ReceiptPending {
		t.Fatalf("new receipt status = %q, want %q", got.Status, store.ReceiptPending)
	}
	if got.Currency != "EUR" || got.Plan != "crew" || got.TierName != "Crew Membership" {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if !got.PaidAt.Equal(now) {
		t.Fatalf("paid_at round-trip: got %v want %v", got.PaidAt, now)
	}

	if _, err := db.GetReceiptByKey(ctx, "no-such-key"); err == nil {
		t.Fatal("GetReceiptByKey on a missing key returned no error")
	}

	// Due now.
	due, err := db.ListDueReceipts(ctx, now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("ListDueReceipts: %v", err)
	}
	if len(due) != 1 || due[0].ID != r.ID {
		t.Fatalf("ListDueReceipts = %d rows, want the one pending receipt", len(due))
	}

	// A failed attempt: still pending, counted, and not due again yet.
	next := now.Add(10 * time.Minute)
	if err := db.MarkReceiptAttempt(ctx, r.ID, "billing system said 502", next); err != nil {
		t.Fatalf("MarkReceiptAttempt: %v", err)
	}
	got, err = db.GetReceiptByKey(ctx, "kofi-msg-1")
	if err != nil {
		t.Fatalf("GetReceiptByKey after attempt: %v", err)
	}
	if got.Status != store.ReceiptPending {
		t.Fatalf("a failed attempt changed status to %q; money that arrived must stay owed a receipt", got.Status)
	}
	if got.Attempts != 1 || got.LastError == "" {
		t.Fatalf("attempt not recorded: attempts=%d err=%q", got.Attempts, got.LastError)
	}
	if due, err = db.ListDueReceipts(ctx, now.Add(time.Minute), 10); err != nil {
		t.Fatalf("ListDueReceipts: %v", err)
	} else if len(due) != 0 {
		t.Fatalf("a receipt backed off to %v came back as due at %v", next, now.Add(time.Minute))
	}
	if due, err = db.ListDueReceipts(ctx, next.Add(time.Second), 10); err != nil {
		t.Fatalf("ListDueReceipts: %v", err)
	} else if len(due) != 1 {
		t.Fatal("a backed-off receipt never became due again")
	}

	// Issued: out of the queue, with the document recorded.
	issuedAt := now.Add(time.Minute)
	if err := db.MarkReceiptIssued(ctx, r.ID, "MSH2026-0001", "inv-hash", issuedAt); err != nil {
		t.Fatalf("MarkReceiptIssued: %v", err)
	}
	got, err = db.GetReceiptByKey(ctx, "kofi-msg-1")
	if err != nil {
		t.Fatalf("GetReceiptByKey after issue: %v", err)
	}
	if got.Status != store.ReceiptIssued || got.InvoiceNumber != "MSH2026-0001" || got.InvoiceRef != "inv-hash" {
		t.Fatalf("issue not recorded: %+v", got)
	}
	if got.IssuedAt == nil || !got.IssuedAt.Equal(issuedAt) {
		t.Fatalf("issued_at = %v, want %v", got.IssuedAt, issuedAt)
	}
	if got.LastError != "" {
		t.Fatalf("a successful issue left the previous error in place: %q", got.LastError)
	}
	if due, err = db.ListDueReceipts(ctx, next.Add(time.Hour), 10); err != nil {
		t.Fatalf("ListDueReceipts: %v", err)
	} else if len(due) != 0 {
		t.Fatal("an issued receipt is still in the drain queue; it would be sent twice")
	}

	// Blocked: parked for a person, and likewise out of the queue.
	blocked := &store.Receipt{
		TenantID:    "t-receipts",
		DeliveryKey: "kofi-msg-2",
		Email:       "payer@example.org",
		AmountCents: 2900,
		Currency:    "USD",
		PaidAt:      now,
	}
	if _, err := db.CreateReceipt(ctx, blocked); err != nil {
		t.Fatalf("CreateReceipt(blocked): %v", err)
	}
	if err := db.BlockReceipt(ctx, blocked.ID, "currency USD is not invoiced by this company"); err != nil {
		t.Fatalf("BlockReceipt: %v", err)
	}
	if due, err = db.ListDueReceipts(ctx, next.Add(time.Hour), 10); err != nil {
		t.Fatalf("ListDueReceipts: %v", err)
	} else if len(due) != 0 {
		t.Fatalf("a blocked receipt is still being retried: %d rows", len(due))
	}
	got, err = db.GetReceiptByKey(ctx, "kofi-msg-2")
	if err != nil {
		t.Fatalf("GetReceiptByKey(blocked): %v", err)
	}
	if got.Status != store.ReceiptBlocked || got.LastError == "" {
		t.Fatalf("block not recorded: %+v", got)
	}

	// A receipt with no delivery key is refused: an empty key is one blank
	// that would collide with every other blank, so the outbox would dedupe
	// unrelated payments into one document.
	if _, err := db.CreateReceipt(ctx, &store.Receipt{TenantID: "t-receipts", PaidAt: now}); err == nil {
		t.Fatal("CreateReceipt accepted an empty delivery key")
	}
}

// testReceiptPaymentRef pins the link a refund needs to find its document.
//
// Stripe removed charge.invoice, and neither the charge nor the payment intent
// points back at an invoice, so a refunded subscription matched no receipt and
// issued no credit note -- the sale stayed in the books at full value with its
// VAT declared. The join is recorded when the payment arrives instead.
func testReceiptPaymentRef(t *testing.T, db store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	r := &store.Receipt{
		TenantID: "t-payref", DeliveryKey: "stripe:inv:in_payref",
		TransactionID: "in_payref", Email: "payer@example.org",
		AmountCents: 900, Currency: "EUR", Plan: "crew", PaidAt: now, NextAttemptAt: now,
	}
	if _, err := db.CreateReceipt(ctx, r); err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}

	// A receipt starts with no payment recorded against it.
	got, err := db.GetReceiptByKey(ctx, r.DeliveryKey)
	if err != nil {
		t.Fatalf("GetReceiptByKey: %v", err)
	}
	if got.PaymentRef != "" {
		t.Errorf("a new receipt carries payment_ref %q", got.PaymentRef)
	}

	// Nothing matches before the link is recorded, and that is reported as a
	// miss rather than as the first row in the table.
	if _, err := db.GetReceiptByPaymentRef(ctx, "pi_payref"); err == nil {
		t.Fatal("an unrecorded payment matched a receipt")
	}
	// An empty reference must never match anything: every un-linked receipt
	// carries one, so a match would attach a credit note to an arbitrary sale.
	if _, err := db.GetReceiptByPaymentRef(ctx, ""); err == nil {
		t.Fatal("an empty payment reference matched a receipt")
	}

	if err := db.SetReceiptPaymentRef(ctx, r.DeliveryKey, "pi_payref"); err != nil {
		t.Fatalf("SetReceiptPaymentRef: %v", err)
	}
	found, err := db.GetReceiptByPaymentRef(ctx, "pi_payref")
	if err != nil {
		t.Fatalf("GetReceiptByPaymentRef after recording: %v", err)
	}
	if found.ID != r.ID {
		t.Errorf("found receipt %q, want %q", found.ID, r.ID)
	}
	if found.PaymentRef != "pi_payref" {
		t.Errorf("payment_ref = %q", found.PaymentRef)
	}

	// Writing the same value twice is the same value: Stripe redelivers, and
	// this is a note on a row rather than an application of money.
	if err := db.SetReceiptPaymentRef(ctx, r.DeliveryKey, "pi_payref"); err != nil {
		t.Fatalf("second SetReceiptPaymentRef: %v", err)
	}
	if again, err := db.GetReceiptByPaymentRef(ctx, "pi_payref"); err != nil || again.ID != r.ID {
		t.Errorf("redelivery changed the link: %v %v", again, err)
	}

	// It survives a round trip through the normal read path.
	back, err := db.GetReceiptByKey(ctx, r.DeliveryKey)
	if err != nil {
		t.Fatalf("GetReceiptByKey: %v", err)
	}
	if back.PaymentRef != "pi_payref" {
		t.Errorf("payment_ref did not survive the round trip: %q", back.PaymentRef)
	}
}
