package ntfy

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
	svc := integrations.New(db, key)
	svc.DisableURLCheckForTest() // httptest binds to loopback; see the method doc
	return svc
}

// recorder records each publish and the Authorization header it carried, so a
// test can tell WHOSE server was used and with whose token.
func recorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), auths...) }
}

// Each tenant publishes to its own ntfy server with its own token. The token
// matters as much as the URL: it is how ntfy authorises a protected topic, and
// one tenant holding another's token would be able to publish into their feed.
func TestEachTenantPublishesToItsOwnServerWithItsOwnToken(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	theirs, theirAuths := recorder(t)
	platform, platformAuths := recorder(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderNtfy,
		map[string]string{"url": theirs.URL, "token": "tenant-token"}); err != nil {
		t.Fatal(err)
	}

	p := New(platform.URL)
	p.SetToken("platform-token")
	pool := NewClientPool(p, accounts)
	pool.noGuard = true // httptest binds to loopback; see the field comment
	n := NewNotifierPool(pool)

	if err := n.Notify(tenancy.WithTenant(ctx, "t_cust"), []string{"alerts"}, "s", "b"); err != nil {
		t.Fatalf("the tenant's own server was not used: %v", err)
	}

	got := theirAuths()
	if len(got) != 1 {
		t.Fatalf("the tenant's server received %d publishes, want 1", len(got))
	}
	if got[0] != "Bearer tenant-token" {
		t.Errorf("published with %q, want the tenant's own token", got[0])
	}
	if n := len(platformAuths()); n != 0 {
		t.Errorf("the operator's ntfy server received %d of a customer's pushes, want 0", n)
	}
}

// A tenant with no ntfy server fails loudly. Silence is the defect this whole
// change exists to fix.
func TestATenantWithNoServerFailsLoudly(t *testing.T) {
	accounts := newAccounts(t)
	platform, platformAuths := recorder(t)
	pool := NewClientPool(New(platform.URL), accounts)
	pool.noGuard = true // httptest binds to loopback; see the field comment
	n := NewNotifierPool(pool)

	if err := n.Notify(tenancy.WithTenant(context.Background(), "t_cust"), []string{"alerts"}, "s", "b"); err == nil {
		t.Fatal("a tenant with no ntfy server reported success: the push went nowhere and nothing said so")
	}
	if n := len(platformAuths()); n != 0 {
		t.Errorf("a tenant with no server was published through the operator's: %d", n)
	}
}

// Rotating the token must rebuild the cached client, or the tenant keeps
// authenticating with the value they just replaced.
func TestRotatingTheTokenRebuildsTheClient(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	theirs, auths := recorder(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderNtfy,
		map[string]string{"url": theirs.URL, "token": "old"}); err != nil {
		t.Fatal(err)
	}
	pool := NewClientPool(nil, accounts)
	pool.noGuard = true // httptest binds to loopback; see the field comment
	n := NewNotifierPool(pool)
	tctx := tenancy.WithTenant(ctx, "t_cust")
	if err := n.Notify(tctx, []string{"alerts"}, "s", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderNtfy,
		map[string]string{"url": theirs.URL, "token": "new"}); err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(tctx, []string{"alerts"}, "s", "b"); err != nil {
		t.Fatal(err)
	}

	got := auths()
	if len(got) != 2 {
		t.Fatalf("got %d publishes, want 2", len(got))
	}
	if got[1] != "Bearer new" {
		t.Errorf("after rotating the token the client still sent %q: the cached "+
			"client outlived the credential it was built from", got[1])
	}
}
