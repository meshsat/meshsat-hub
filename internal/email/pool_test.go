package email

import (
	"context"
	"strings"
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
		key[i] = byte(i * 11)
	}
	return integrations.New(db, key)
}

// THE bug this tranche exists to remove. The keyring was one process-wide map
// keyed by bare email address, so two tenants adding a contact for the same
// address collided: the second overwrote the first, and tenant A's alert to
// alice was then encrypted to the key tenant B supplied -- which tenant B can
// decrypt and read. MESHSAT-1116 gated the endpoints to platform admins as a
// stopgap; this removes the defect instead.
func TestTwoTenantsContactsForTheSameAddressDoNotCollide(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	for _, tid := range []string{"t_one", "t_two"} {
		if _, err := accounts.Set(ctx, tid, integrations.ProviderEmail, map[string]string{
			"webhook_secret": "s-" + tid, "smtp_host": "localhost:2525", "from": tid + "@example.org",
		}); err != nil {
			t.Fatal(err)
		}
	}

	p := NewPool(nil, accounts)
	one, two := p.ForTenant(ctx, "t_one"), p.ForTenant(ctx, "t_two")
	if one == nil || two == nil {
		t.Fatal("a tenant with SMTP details got no gateway")
	}
	if one.KeyRing == two.KeyRing {
		t.Fatal("two tenants share one keyring: a contact added by either " +
			"overwrites the other's key for the same address")
	}

	// Each tenant registers a DIFFERENT key for the same correspondent.
	keyOne := generateTestKey(t, "alice@example.com")
	keyTwo := generateTestKey(t, "alice@example.com")
	if err := one.KeyRing.AddContact("alice@example.com", keyOne); err != nil {
		t.Fatal(err)
	}
	if err := two.KeyRing.AddContact("alice@example.com", keyTwo); err != nil {
		t.Fatal(err)
	}

	gotOne := one.KeyRing.GetContact("alice@example.com")
	gotTwo := two.KeyRing.GetContact("alice@example.com")
	if gotOne == nil || gotTwo == nil {
		t.Fatal("a contact went missing")
	}
	if gotOne.PrimaryKey.KeyId == gotTwo.PrimaryKey.KeyId {
		t.Fatal("one tenant's contact key overwrote the other's for the same " +
			"address: mail to that correspondent would be encrypted to a key the " +
			"OTHER tenant supplied and can read")
	}

	// And neither tenant can see the other's correspondent list.
	if len(one.KeyRing.ListContacts()) != 1 || len(two.KeyRing.ListContacts()) != 1 {
		t.Errorf("contact lists leak across tenants: %v / %v",
			one.KeyRing.ListContacts(), two.KeyRing.ListContacts())
	}
}

// A tenant with only a webhook secret can receive but has nothing to send with,
// and must not inherit the operator's sending identity.
func TestAnInboundOnlyTenantGetsNoSendingGateway(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()

	if _, err := accounts.Set(ctx, "t_inbound", integrations.ProviderEmail,
		map[string]string{"webhook_secret": "abc"}); err != nil {
		t.Fatal(err)
	}

	kr, err := NewKeyRing("MeshSat Hub", "platform@example.org", "")
	if err != nil {
		t.Fatal(err)
	}
	platform := &Gateway{Client: NewClient("localhost:2525", "platform@example.org", "", "", kr), KeyRing: kr}
	p := NewPool(platform, accounts)

	if gw := p.ForTenant(ctx, "t_inbound"); gw != nil {
		t.Fatal("a tenant with no SMTP details was handed a sending gateway: " +
			"its alerts would leave from the operator's address, signed with the " +
			"operator's PGP key")
	}
	if gw := p.ForTenant(ctx, "default"); gw == nil {
		t.Fatal("the default tenant lost the platform gateway")
	}
}

// generateTestKey makes a throwaway armored public key for an address.
func generateTestKey(t *testing.T, addr string) string {
	t.Helper()
	kr, err := NewKeyRing("Test", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	pub := kr.HubPublicKey()
	if !strings.Contains(pub, "BEGIN PGP PUBLIC KEY BLOCK") {
		t.Fatalf("not an armored public key: %q", pub[:40])
	}
	return pub
}
