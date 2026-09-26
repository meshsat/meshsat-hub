package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The Postgres column is VARCHAR(2) and refused "USA" as SQLSTATE 22001 at the
// bottom of a customer's first sign-in (MESHSAT-1365). sqlite enforces no
// length, so the unit tests never saw it. Both backends now refuse the same
// named error, before the row is touched.
func TestTenantBillingCountryMustBeAlpha2(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	bad := &store.Tenant{ID: "t-usa", Slug: "usa", Name: "USA", BillingCountry: "USA"}
	if err := db.CreateTenant(ctx, bad); !errors.Is(err, store.ErrBillingCountry) {
		t.Fatalf("CreateTenant with %q: err = %v, want ErrBillingCountry", bad.BillingCountry, err)
	}
	if got, err := db.GetTenant(ctx, "t-usa"); err == nil && got != nil {
		t.Fatalf("a refused tenant was written: %+v", got)
	}

	good := &store.Tenant{ID: "t-us", Slug: "us", Name: "US", BillingCountry: "US"}
	if err := db.CreateTenant(ctx, good); err != nil {
		t.Fatalf("CreateTenant with a valid code: %v", err)
	}
	good.BillingCountry = "United States"
	if err := db.UpdateTenant(ctx, good); !errors.Is(err, store.ErrBillingCountry) {
		t.Fatalf("UpdateTenant with %q: err = %v, want ErrBillingCountry", good.BillingCountry, err)
	}
	good.BillingCountry = ""
	if err := db.UpdateTenant(ctx, good); err != nil {
		t.Fatalf("UpdateTenant clearing the country: %v", err)
	}
}
