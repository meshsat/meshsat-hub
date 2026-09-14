package apprise

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

func newAccounts(t *testing.T) *integrations.Service {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 5)
	}
	return integrations.New(db, key)
}

// recorder is a stand-in Apprise server that records that it was asked to
// deliver something.
func recorder(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// Each tenant's alerts go through ITS OWN relay. The targets were always per
// tenant; the relay that delivers to them was one global value, so before
// MESHSAT-1121 every tenant's alert went through the operator's server -- or,
// in production, through nothing at all.
func TestEachTenantIsDeliveredThroughItsOwnRelay(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	theirs, theirCount := recorder(t)
	platform, platformCount := recorder(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderApprise,
		map[string]string{"url": theirs.URL}); err != nil {
		t.Fatal(err)
	}

	n := NewNotifierPool(NewClientPool(New(platform.URL), accounts))

	if err := n.Notify(tenancy.WithTenant(ctx, "t_cust"), []string{"mailto://x"}, "s", "b"); err != nil {
		t.Fatalf("the tenant's own relay was not used: %v", err)
	}
	if theirCount() != 1 {
		t.Errorf("the tenant's relay received %d notifications, want 1", theirCount())
	}
	if platformCount() != 0 {
		t.Errorf("the operator's relay received %d of a customer's notifications, want 0: "+
			"one tenant's alerts are being delivered through another's server", platformCount())
	}
}

// A tenant with no relay must get an ERROR, not silence. This is the exact
// defect the audit found: /api/notifications/prefs accepted a URL, answered 200,
// and nothing anywhere was configured to deliver it.
func TestATenantWithNoRelayFailsLoudlyRatherThanSilently(t *testing.T) {
	accounts := newAccounts(t)
	platform, platformCount := recorder(t)
	n := NewNotifierPool(NewClientPool(New(platform.URL), accounts))

	err := n.Notify(tenancy.WithTenant(context.Background(), "t_cust"), []string{"mailto://x"}, "s", "b")
	if err == nil {
		t.Fatal("a tenant with no notification relay reported success: " +
			"the customer's alert went nowhere and nothing said so")
	}
	if platformCount() != 0 {
		t.Errorf("a tenant with no relay was delivered through the operator's: %d", platformCount())
	}
}

// The default tenant still uses the platform relay, so an existing single-tenant
// or self-hosted Hub behaves exactly as before.
func TestTheDefaultTenantStillUsesThePlatformRelay(t *testing.T) {
	accounts := newAccounts(t)
	platform, platformCount := recorder(t)
	n := NewNotifierPool(NewClientPool(New(platform.URL), accounts))

	if err := n.Notify(tenancy.WithTenant(context.Background(), "default"),
		[]string{"mailto://x"}, "s", "b"); err != nil {
		t.Fatalf("the default tenant could not notify: %v", err)
	}
	if platformCount() != 1 {
		t.Errorf("the platform relay received %d, want 1", platformCount())
	}
}
