package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// testBuyerCountry pins that where the buyer is survives a round trip through a
// real database in both dialects, and that the cross-border total the flat
// Dutch rate depends on is measured from issued receipts only.
//
// Before this, nothing anywhere held a buyer's country: every customer was
// written into the billing system as Dutch and charged 21%, which is right for
// the Netherlands and wrong for anybody outside the EU (MESHSAT-1016).
func testBuyerCountry(t *testing.T, s store.Store) {
	ctx := context.Background()

	tn := &store.Tenant{
		ID: "bc-one", Slug: "bc-one", Name: "Dieter", Plan: plans.Free, Status: store.TenantActive,
		BillingCountry: "DE", BillingCountryEvidence: "declared DE, seen from 203.0.113.9 at sign-up",
	}
	if err := s.CreateTenant(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	got, err := s.GetTenant(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BillingCountry != "DE" {
		t.Errorf("billing country round-tripped as %q, want DE", got.BillingCountry)
	}
	if got.BillingCountryEvidence == "" {
		t.Error("the evidence for the country was not stored; the rules ask for it, not just the answer")
	}

	// It survives an update too, which is where a column silently dropped from
	// an UPDATE statement would show up.
	got.Name = "Dieter renamed"
	if err := s.UpdateTenant(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetTenant(ctx, tn.ID)
	if again.BillingCountry != "DE" {
		t.Errorf("an unrelated update lost the billing country: %q", again.BillingCountry)
	}

	// Receipts carry a frozen copy: a customer who moves next year must not
	// change the VAT on a document that was already issued.
	now := time.Now().UTC()
	seed := []struct {
		id, country, status string
		cents               int64
	}{
		{"bc-de", "DE", store.ReceiptIssued, 120000},
		{"bc-ie", "IE", store.ReceiptIssued, 30000},
		{"bc-nl", "NL", store.ReceiptIssued, 500000},     // domestic: excluded by the caller
		{"bc-pending", "FR", store.ReceiptPending, 9999}, // not issued: not a supply yet
	}
	// A donation from an EU giver. Issued, and deliberately large, so that a
	// query which forgot to exclude it could not possibly pass below.
	donation := &store.Receipt{
		ID: "bc-gift", TenantID: tn.ID, DeliveryKey: "k-bc-gift", Country: "BE",
		Email: "giver@example.com", AmountCents: 900000, Currency: "EUR",
		Plan: store.DonationPlan, Status: store.ReceiptIssued, PaidAt: now, NextAttemptAt: now,
	}
	if created, err := s.CreateReceipt(ctx, donation); err != nil || !created {
		t.Fatalf("seed donation: created=%v err=%v", created, err)
	}
	if err := s.MarkReceiptIssued(ctx, donation.ID, "MSH2026-bc-gift", "ref-bc-gift", now); err != nil {
		t.Fatalf("mark donation issued: %v", err)
	}
	for _, sd := range seed {
		r := &store.Receipt{
			ID: sd.id, TenantID: tn.ID, DeliveryKey: "k-" + sd.id, Country: sd.country,
			Email: "b@example.com", AmountCents: sd.cents, Currency: "EUR", Plan: "crew",
			Status: sd.status, PaidAt: now, NextAttemptAt: now,
		}
		if created, err := s.CreateReceipt(ctx, r); err != nil || !created {
			t.Fatalf("seed %s: created=%v err=%v", sd.id, created, err)
		}
		if sd.status == store.ReceiptIssued {
			if err := s.MarkReceiptIssued(ctx, sd.id, "MSH2026-"+sd.id, "ref-"+sd.id, now); err != nil {
				t.Fatalf("mark issued %s: %v", sd.id, err)
			}
		}
	}

	byCountry, err := s.CrossBorderSalesSince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("cross-border sales: %v", err)
	}
	if byCountry["DE"] != 120000 || byCountry["IE"] != 30000 {
		t.Errorf("issued EU sales = %v, want DE 120000 and IE 30000", byCountry)
	}
	if _, ok := byCountry["FR"]; ok {
		t.Error("a receipt that has not been issued counts as a supply; it must not")
	}
	// NL is returned; excluding it is internal/vat's job, not the store's, and
	// the caller must be able to see the domestic figure separately.
	if byCountry["NL"] != 500000 {
		t.Errorf("domestic total = %d, want it reported so the caller can exclude it", byCountry["NL"])
	}
	// A donation is NOT a supply. The Article 59c threshold measures
	// cross-border B2C supplies, and a voluntary contribution with no
	// counter-performance carries no VAT and belongs in no member state's
	// column -- including it would report distance travelled toward a limit
	// that the money cannot move.
	if n, ok := byCountry["BE"]; ok {
		t.Errorf("a donation counted toward the VAT threshold: BE = %d", n)
	}
}
