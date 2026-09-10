// Package stripe is the Hub's payment provider: it starts a checkout, and it
// listens to what happens afterwards.
//
// It replaces internal/kofi (MESHSAT-1023). The predecessor could be told that
// money had arrived and nothing else -- no cancellation, no refund, no country,
// and no way to say which tenant a payment was for except an eight-character
// code the customer had to type into a message box. Three of the more awkward
// mechanisms in this codebase existed only to work around that. Every one of
// them is a webhook event here.
//
// What this package is NOT:
//
//   - It is not the book. Invoice Ninja issues every numbered, customer-facing
//     document; Stripe objects are payment records. Stripe Tax is off and stays
//     off, and no invoice or payment link may be raised in Stripe (owner ruling,
//     2026-09-10). Every request this package makes sends
//     automatic_tax[enabled]=false explicitly rather than trusting a dashboard
//     setting somebody may flip.
//   - It is not on any ingest path. A distress message must never depend on a
//     billing decision; sos_invariant_test.go enforces that mechanically.
//   - It does not decide VAT. internal/vat owns that rule and this package feeds
//     it a country.
//
// No SDK. Six calls and one signature check against a codebase with seventeen
// direct dependencies and an explicit dependency-minimalism rule, next to an
// internal/invoiceninja that is hand-rolled for the same reason.
package stripe

import (
	"context"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// ErrNotConfigured means no API key was supplied, so nothing is called. A nil
// *Client is safe: it reports this rather than panicking, which is what lets
// the Hub run with billing switched off.
var ErrNotConfigured = errors.New("stripe: not configured")

// ErrNoTenant means an event carried no tenant this Hub knows. It is never
// guessed at: a payment attributed to the wrong tenant is worse than one
// attributed to none, and the money is recorded for a person either way.
var ErrNoTenant = errors.New("stripe: no tenant on this event")

// TenantStore is the slice of the store this needs. Deliberately narrow: this
// package may move a plan and record a payment, and must not be able to reach
// a device, a bridge or a message from here.
type TenantStore interface {
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
	UpdateTenant(ctx context.Context, t *store.Tenant) error
	// TenantByStripeCustomer resolves the events that carry a customer but no
	// metadata of their own.
	TenantByStripeCustomer(ctx context.Context, customerID string) (*store.Tenant, error)
	// ApplyStripeEvent records an event id and reports whether this call was
	// the one that recorded it. A compare-and-set, not a read followed by a
	// write: two replicas serving the same retry would otherwise both find it
	// missing and both apply it. Stripe redelivers until it gets a 2xx, so this
	// is not a rare path.
	ApplyStripeEvent(ctx context.Context, eventID, tenantID string) (bool, error)
}

// Auditor records what was done and why.
type Auditor interface {
	Log(ctx context.Context, tenantID, action, actor, detail, ip string) error
}

// Clock is injected so tests can pin the signature tolerance window.
type Clock func() time.Time
