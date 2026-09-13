package sos

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// MESHSAT-1115. resolveChainID returned the configured chain id for EVERY
// tenant, without looking at the tenant at all. escalation_chains rows are
// tenant-scoped, so on the hosted Hub that would route every customer's SOS to
// a chain id that exists only in one tenant: GetEscalationChain fails and the
// alert is closed undelivered. Silently, for everyone but the default tenant.
//
// HUB_SOS_CHAIN_ID stays supported, because a self-hosted single-tenant
// deployment is a real case for an Apache-2.0 Hub. It is scoped to the one
// tenant it can name.
func newDetectorStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTheConfiguredChainIsUsedOnlyForTheDefaultTenant(t *testing.T) {
	st := newDetectorStore(t)
	resolver := tenancy.NewResolver(st, store.DefaultTenantID, time.Minute)
	d := NewDetector(&recordingBus{}, nil, st, resolver, "chain-from-config")

	if got := d.resolveChainID(store.DefaultTenantID); got != "chain-from-config" {
		t.Errorf("default tenant got %q, want the configured chain", got)
	}

	// A customer tenant with no chain of its own must NOT inherit it. Empty is
	// the correct answer: the detector then logs "no escalation chain
	// configured" and raises no alert, rather than raising one that cannot be
	// delivered and is closed undelivered on the next tick.
	if got := d.resolveChainID("t_customer"); got != "" {
		t.Errorf("customer tenant got %q, want \"\" -- a chain id from another tenant "+
			"cannot be loaded and the SOS would be dropped without notifying anyone", got)
	}
}

// And a tenant with its own chain gets its own, which is the multi-tenant path
// and needs no configuration at all.
func TestATenantWithItsOwnChainGetsIt(t *testing.T) {
	st := newDetectorStore(t)
	ctx := context.Background()
	resolver := tenancy.NewResolver(st, store.DefaultTenantID, time.Minute)
	d := NewDetector(&recordingBus{}, nil, st, resolver, "chain-from-config")

	own := &store.EscalationChain{
		Name:  "customer's own",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"+31600000000"}, MaxRetries: 1}},
	}
	if err := st.CreateEscalationChain(ctx, "t_customer", own); err != nil {
		t.Fatalf("chain: %v", err)
	}

	if got := d.resolveChainID("t_customer"); got != own.ID {
		t.Errorf("got %q, want the tenant's own chain %q", got, own.ID)
	}
	// The default tenant still gets the configured one, not the customer's.
	if got := d.resolveChainID(store.DefaultTenantID); got != "chain-from-config" {
		t.Errorf("default tenant got %q, want the configured chain", got)
	}
}
