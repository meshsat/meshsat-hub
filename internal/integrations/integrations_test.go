package integrations

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func newSvc(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return New(db, key), ctx
}

func TestSetGetMaskDelete(t *testing.T) {
	s, ctx := newSvc(t)
	if a, err := s.ForTenant(ctx, "t1", ProviderCloudloop); err != nil || a != nil {
		t.Fatalf("unconfigured: %v %v", a, err)
	}
	a, err := s.Set(ctx, "t1", ProviderCloudloop, map[string]string{"api_key": "k-123456789", "account_id": "acc"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if a.Get("api_url") != "https://api.cloudloop.com" {
		t.Errorf("default api_url missing: %q", a.Get("api_url"))
	}
	if a.Get("webhook_token") == "" {
		t.Errorf("webhook_token not generated")
	}
	got, err := s.ForTenant(ctx, "t1", ProviderCloudloop)
	if err != nil || got == nil || got.Get("api_key") != "k-123456789" || got.Platform {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// Update a non-secret field without resending the secret: the key survives.
	if _, err := s.Set(ctx, "t1", ProviderCloudloop, map[string]string{"account_id": "acc2", "api_key": ""}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.ForTenant(ctx, "t1", ProviderCloudloop)
	if got.Get("api_key") != "k-123456789" || got.Get("account_id") != "acc2" || got.Get("webhook_token") != a.Get("webhook_token") {
		t.Errorf("update lost data: %+v", got.Fields)
	}
	spec, _ := SpecFor(ProviderCloudloop)
	m := Masked(spec, got)
	if m["api_key"] != "••••6789" || m["account_id"] != "acc2" {
		t.Errorf("masked = %v", m)
	}
	// Rows are stored encrypted.
	creds, _ := s.store.ListCredentials(ctx, "t1")
	if len(creds) != 1 || creds[0].CredType != CredType || creds[0].TargetScope != Scope || string(creds[0].EncryptedData) == "" || contains(creds[0].EncryptedData, "k-123456789") {
		t.Errorf("stored row wrong: %+v", creds)
	}
	if err := s.Delete(ctx, "t1", ProviderCloudloop); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if a, _ := s.ForTenant(ctx, "t1", ProviderCloudloop); a != nil {
		t.Errorf("still configured after delete")
	}
	if _, err := s.Set(ctx, "t1", ProviderTwilio, map[string]string{"account_sid": "AC1"}); err == nil {
		t.Errorf("missing required fields accepted")
	}
	if _, err := s.Set(ctx, "t1", "nope", nil); err != ErrUnknownProvider {
		t.Errorf("unknown provider: %v", err)
	}
}

func contains(b []byte, s string) bool {
	return len(b) >= len(s) && string(b) != "" && indexOf(string(b), s) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestPlatformFallbackAndTokenLookup(t *testing.T) {
	s, ctx := newSvc(t)
	s.SetPlatform(ProviderCloudloop, map[string]string{"api_key": "platform-key", "webhook_token": "platform-token"})
	s.SetPlatform(ProviderTwilio, map[string]string{"account_sid": "AC"}) // incomplete: ignored
	if s.Platform(ProviderTwilio) != nil {
		t.Errorf("incomplete platform account kept")
	}
	a, err := s.ForTenant(ctx, store.DefaultTenantID, ProviderCloudloop)
	if err != nil || a == nil || !a.Platform || a.Get("api_key") != "platform-key" {
		t.Fatalf("default fallback: %+v %v", a, err)
	}
	if a, _ := s.ForTenant(ctx, "t2", ProviderCloudloop); a != nil {
		t.Errorf("non-default tenant must not get the platform account")
	}
	t2, _ := s.Set(ctx, "t2", ProviderCloudloop, map[string]string{"api_key": "k2"})
	t3, _ := s.Set(ctx, "t3", ProviderCloudloop, map[string]string{"api_key": "k3"})

	tenant, acct, err := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", t3.Get("webhook_token"))
	if err != nil || tenant != "t3" || acct.Get("api_key") != "k3" {
		t.Fatalf("lookup t3: %s %+v %v", tenant, acct, err)
	}
	if tenant, _, _ := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", t2.Get("webhook_token")); tenant != "t2" {
		t.Errorf("lookup t2 = %s", tenant)
	}
	if tenant, _, _ := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", "platform-token"); tenant != store.DefaultTenantID {
		t.Errorf("platform token -> %s", tenant)
	}
	if tenant, a, _ := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", "nope"); tenant != "" || a != nil {
		t.Errorf("unknown token matched %s", tenant)
	}
	// Cached hit is re-verified: rotating the token invalidates the old one.
	_, _ = s.Set(ctx, "t3", ProviderCloudloop, map[string]string{"webhook_token": "rotated-token-value"})
	if tenant, _, _ := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", t3.Get("webhook_token")); tenant != "" {
		t.Errorf("old token still resolves to %s", tenant)
	}
	if tenant, _, _ := s.LookupByToken(ctx, ProviderCloudloop, "webhook_token", "rotated-token-value"); tenant != "t3" {
		t.Errorf("rotated token -> %s", tenant)
	}
}
