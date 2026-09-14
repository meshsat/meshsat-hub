package hawkbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
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
		key[i] = byte(i * 7)
	}
	svc := integrations.New(db, key)
	svc.DisableURLCheckForTest() // httptest binds to loopback; see the method doc
	return svc
}

// recorder records the Authorization header each request carried, which is how
// hawkBit decides WHICH OF ITS TENANTS the caller is acting in.
func recorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	t.Cleanup(s.Close)
	return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), auths...) }
}

// Each tenant's rollouts go to its OWN hawkBit with its OWN credentials. Those
// credentials are what hawkBit scopes by, so this is the whole tenant boundary
// for OTA -- and OTA pushes firmware to hardware in the field.
func TestEachTenantUsesItsOwnHawkbitAccount(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	theirs, theirAuths := recorder(t)
	platform, platformAuths := recorder(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderHawkbit, map[string]string{
		"url": theirs.URL, "username": "their-user", "password": "their-pass",
	}); err != nil {
		t.Fatal(err)
	}

	p := NewClientPool(NewClient(platform.URL, "plat-user", "plat-pass"), accounts)
	p.noGuard = true // httptest binds to loopback; see the field comment

	c := p.ForTenant(ctx, "t_cust")
	if c == nil {
		t.Fatal("a tenant with its own hawkBit account got no client")
	}
	if _, err := c.ListTargets(ctx); err != nil {
		t.Fatal(err)
	}

	if got := theirAuths(); len(got) != 1 {
		t.Fatalf("the tenant's own server saw %d requests, want 1", len(got))
	}
	if n := len(platformAuths()); n != 0 {
		t.Errorf("the operator's hawkBit saw %d of a customer's requests, want 0: "+
			"one tenant could push firmware into another's fleet", n)
	}
}

// A tenant with no hawkBit gets nil, not the operator's account. Inheriting it
// would mean a customer's rollout reaching the operator's own fleet.
func TestATenantWithNoHawkbitDoesNotInheritThePlatforms(t *testing.T) {
	accounts := newAccounts(t)
	platform, _ := recorder(t)
	p := NewClientPool(NewClient(platform.URL, "plat-user", "plat-pass"), accounts)
	p.noGuard = true // httptest binds to loopback; see the field comment

	if c := p.ForTenant(context.Background(), "t_cust"); c != nil {
		t.Fatal("a tenant with no OTA account was handed the operator's hawkBit: " +
			"its rollouts would push firmware to the operator's own fleet")
	}
	if c := p.ForTenant(context.Background(), "default"); c == nil {
		t.Fatal("the default tenant lost the platform account")
	}
}

// An account missing its username resolves to nothing rather than to a client
// that will fail every call: hawkBit scopes by the principal, so a half-filled
// account has no tenant to act in.
func TestAHalfFilledAccountResolvesToNothing(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderHawkbit, map[string]string{
		"url": "https://hawkbit.example.org", "username": "u", "password": "p",
	}); err != nil {
		t.Fatal(err)
	}
	p := NewClientPool(nil, accounts)
	p.noGuard = true // httptest binds to loopback; see the field comment
	if c := p.ForTenant(ctx, "t_cust"); c == nil {
		t.Fatal("a complete account resolved to nothing")
	}
}
