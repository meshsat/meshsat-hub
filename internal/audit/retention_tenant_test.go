package audit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1117 tranche 2b. purgeAll computed ONE cutoff from
// HUB_AUDIT_RETENTION_DAYS and applied it to every tenant. An audit log is the
// customer's own record of who did what in their account, and how long it needs
// keeping is their compliance question.
//
// These tests are about the job rather than the API, because the job is the
// thing that deletes data: a wrong cutoff here destroys a tenant's security
// history, and an append-only hash chain cannot be restored from the next row.

// perTenantStore records the cutoff each tenant was purged with.
type perTenantStore struct {
	mu      sync.Mutex
	tenants []store.Tenant
	cutoffs map[string]time.Time
	listErr error
}

func (f *perTenantStore) ListTenants(context.Context) ([]store.Tenant, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tenants, nil
}

func (f *perTenantStore) ListAuditEntriesBefore(context.Context, string, time.Time, int) ([]store.AuditEntry, error) {
	return nil, nil
}

func (f *perTenantStore) DeleteAuditEntriesBefore(_ context.Context, tenantID string, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cutoffs == nil {
		f.cutoffs = map[string]time.Time{}
	}
	f.cutoffs[tenantID] = before
	return 0, nil
}

// daysBack reports roughly how many days before now a cutoff is.
func cutoffDaysBack(cutoff time.Time) int {
	return int(time.Since(cutoff).Round(24*time.Hour) / (24 * time.Hour))
}

func TestEachTenantIsPurgedAgainstItsOwnRetention(t *testing.T) {
	f := &perTenantStore{tenants: []store.Tenant{
		{ID: store.DefaultTenantID},                       // chose nothing
		{ID: "t_short", AuditRetentionDays: 60},           // its own, inside the bounds
		{ID: "t_long", AuditRetentionDays: 365},           // its own, longer
		{ID: "t_default_explicit", AuditRetentionDays: 0}, // 0 = platform default
	}}
	cfg := RetentionConfig{RetentionDays: 90, MinDays: 30, MaxDays: 3650}

	purgeAll(context.Background(), f, cfg, nil)

	for tenant, want := range map[string]int{
		store.DefaultTenantID: 90,
		"t_short":             60,
		"t_long":              365,
		"t_default_explicit":  90,
	} {
		got, ok := f.cutoffs[tenant]
		if !ok {
			t.Errorf("%s was never purged", tenant)
			continue
		}
		if d := cutoffDaysBack(got); d != want {
			t.Errorf("%s purged at %d days, want %d. One tenant's retention was applied "+
				"to another's audit log.", tenant, d, want)
		}
	}
}

// A stored value outside the platform's bounds must be clamped here too. The
// API refuses one, but a row can also be written by hand, and a zero or
// negative cutoff would purge a tenant's entire audit log on the next tick.
func TestAStoredRetentionOutsideTheBoundsIsClamped(t *testing.T) {
	f := &perTenantStore{tenants: []store.Tenant{
		{ID: "t_tiny", AuditRetentionDays: 1},
		{ID: "t_huge", AuditRetentionDays: 999999},
	}}
	cfg := RetentionConfig{RetentionDays: 90, MinDays: 30, MaxDays: 3650}

	purgeAll(context.Background(), f, cfg, nil)

	if d := cutoffDaysBack(f.cutoffs["t_tiny"]); d != 30 {
		t.Errorf("a one-day retention purged at %d days, want the floor of 30. A row written "+
			"by hand could otherwise erase a tenant's security history.", d)
	}
	if d := cutoffDaysBack(f.cutoffs["t_huge"]); d != 3650 {
		t.Errorf("an unbounded retention purged at %d days, want the ceiling of 3650", d)
	}
}

// Retention off at the platform means off for a tenant that chose nothing, and
// a tenant's own choice still applies.
func TestRetentionDisabledPlatformWideStillHonoursATenantsChoice(t *testing.T) {
	f := &perTenantStore{tenants: []store.Tenant{
		{ID: store.DefaultTenantID},
		{ID: "t_keeps_its_own", AuditRetentionDays: 60},
	}}
	cfg := RetentionConfig{RetentionDays: 0, MinDays: 30, MaxDays: 3650}

	purgeAll(context.Background(), f, cfg, nil)

	if _, purged := f.cutoffs[store.DefaultTenantID]; purged {
		t.Error("a tenant that chose nothing was purged while platform retention is disabled")
	}
	if d := cutoffDaysBack(f.cutoffs["t_keeps_its_own"]); d != 60 {
		t.Errorf("the tenant's own 60-day retention was not applied (got %d days)", d)
	}
}

// The default tenant is seeded before the listing, and its own row must still
// win -- otherwise the platform tenant is the one account that can never
// choose its own retention.
func TestTheDefaultTenantsOwnChoiceIsNotOverwrittenBySeeding(t *testing.T) {
	f := &perTenantStore{tenants: []store.Tenant{
		{ID: store.DefaultTenantID, AuditRetentionDays: 120},
	}}
	cfg := RetentionConfig{RetentionDays: 90, MinDays: 30, MaxDays: 3650}

	purgeAll(context.Background(), f, cfg, nil)

	if d := cutoffDaysBack(f.cutoffs[store.DefaultTenantID]); d != 120 {
		t.Errorf("the default tenant purged at %d days, want its own 120", d)
	}
	if n := len(f.cutoffs); n != 1 {
		t.Errorf("purged %d tenants, want 1 -- the default tenant was listed twice", n)
	}
}

// If the tenant list cannot be read, the default tenant is still purged on the
// platform default rather than the job silently doing nothing.
func TestAFailedTenantListStillPurgesTheDefaultTenant(t *testing.T) {
	f := &perTenantStore{listErr: context.DeadlineExceeded}
	cfg := RetentionConfig{RetentionDays: 90, MinDays: 30, MaxDays: 3650}

	purgeAll(context.Background(), f, cfg, nil)

	if d := cutoffDaysBack(f.cutoffs[store.DefaultTenantID]); d != 90 {
		t.Errorf("purged at %d days, want the platform default of 90", d)
	}
}
