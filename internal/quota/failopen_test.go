package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The ceiling fails OPEN, on purpose, and nothing pinned that. A count query
// that times out must not stop a customer registering their kit: a fleet
// nobody can add to is a worse failure than a tenant briefly one device over,
// and the ceiling is a billing rule, not a safety one.
func TestTheCeilingFailsOpenWhenItCannotTell(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTenant(t.Context(), &store.Tenant{
		ID: "t1", Slug: "t1", Name: "t1", Plan: plans.Free, Status: store.TenantActive,
	}); err != nil {
		t.Fatal(err)
	}
	// Fill the free plan, so the only thing that can let a registration
	// through is the failure itself.
	for i := 0; i < 4; i++ {
		if err := db.CreateDevice(t.Context(), "t1", &store.Device{
			IMEI: "30000000000000" + string(rune('1'+i)), Label: "d", Type: "rockblock",
		}); err != nil {
			t.Fatal(err)
		}
	}

	full := quota.New(db, func(context.Context, string) (string, error) { return plans.Free, nil })
	if ok, _ := full.AllowAnother(t.Context(), "t1"); ok {
		t.Fatal("a full free plan allowed another registration; the rest of this test is meaningless")
	}

	// Now the same tenant, with the plan lookup broken.
	broken := quota.New(db, func(context.Context, string) (string, error) {
		return "", errors.New("connection reset by peer")
	})
	ok, why := broken.AllowAnother(t.Context(), "t1")
	if !ok {
		t.Errorf("a lookup failure refused a registration: %q -- the ceiling must fail open", why)
	}
	if why != "" {
		t.Errorf("a fail-open answer carried a refusal message: %q", why)
	}

	// A checker with no store at all is a deployment with no tiers configured,
	// which must behave as no ceiling rather than as a ceiling of zero.
	if ok, _ := quota.New(nil, nil).AllowAnother(t.Context(), "t1"); !ok {
		t.Error("an unconfigured checker refused a registration")
	}
	var nilChecker *quota.Checker
	if ok, _ := nilChecker.AllowAnother(t.Context(), "t1"); !ok {
		t.Error("a nil checker refused a registration")
	}
}

// A plan lapses by a date passing, and the hourly job is what writes the
// downgrade. Until it runs the tenant keeps the ceiling it paid for -- the
// check reads tenants.plan, not the expiry. Anything else would drop a paying
// customer to four devices the instant their renewal was a minute late.
func TestALapsedPlanKeepsItsCeilingUntilTheJobRuns(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-48 * time.Hour)
	if err := db.CreateTenant(t.Context(), &store.Tenant{
		ID: "t-late", Slug: "t-late", Name: "t-late", Plan: plans.Crew,
		Status: store.TenantActive, PlanExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := db.CreateDevice(t.Context(), "t-late", &store.Device{
			IMEI: "30000000000010" + string(rune('1'+i)), Label: "d", Type: "rockblock",
		}); err != nil {
			t.Fatal(err)
		}
	}

	q := quota.New(db, func(ctx context.Context, id string) (string, error) {
		tn, err := db.GetTenant(ctx, id)
		if err != nil {
			return plans.Free, err
		}
		return tn.Plan, nil
	})
	u, err := q.Usage(t.Context(), "t-late")
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != plans.Crew || u.Limit != plans.For(plans.Crew).Devices {
		t.Errorf("usage = plan %q limit %d; an expired date must not change the ceiling before the job runs", u.Plan, u.Limit)
	}
	if ok, why := q.AllowAnother(t.Context(), "t-late"); !ok {
		t.Errorf("a tenant whose renewal is late was refused: %q", why)
	}
	if strings.Contains(u.Plan, plans.Free) {
		t.Error("the plan was read as free before the lapse job wrote it")
	}
}
