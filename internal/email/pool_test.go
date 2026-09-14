package email

import (
	"context"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// newContactDB is a store for the contact persistence tests.
func newContactDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

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

// A contact survives a restart. Before MESHSAT-1123 the keyring was a Go map and
// nothing else, so every rollout emptied it -- and the symptom was not an error
// but alert mail quietly reverting to cleartext.
func TestAContactSurvivesARestart(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	db := newContactDB(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderEmail, map[string]string{
		"webhook_secret": "s", "smtp_host": "localhost:2525", "from": "t@example.org",
	}); err != nil {
		t.Fatal(err)
	}

	before := NewPool(nil, accounts)
	before.SetContactStore(db)
	gw := before.ForTenant(ctx, "t_cust")
	if gw == nil {
		t.Fatal("no gateway")
	}
	key := generateTestKey(t, "alice@example.com")
	if err := gw.KeyRing.AddContact("alice@example.com", key); err != nil {
		t.Fatal(err)
	}
	if err := before.PersistContact(ctx, "t_cust", "alice@example.com", key); err != nil {
		t.Fatal(err)
	}

	// A whole new process: new pool, new cache, nothing carried over.
	after := NewPool(nil, accounts)
	after.SetContactStore(db)
	gw2 := after.ForTenant(ctx, "t_cust")
	if gw2 == nil {
		t.Fatal("no gateway after the restart")
	}
	if gw2.KeyRing.GetContact("alice@example.com") == nil {
		t.Fatal("the contact key did not survive a restart: every rollout would " +
			"silently drop this tenant back to sending alerts in cleartext")
	}
}

// The Hub runs two replicas. A key added on one must reach the other, or whether
// an alert is encrypted depends on which pod the load balancer picked.
func TestAContactAddedOnOneReplicaReachesTheOther(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	db := newContactDB(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderEmail, map[string]string{
		"webhook_secret": "s", "smtp_host": "localhost:2525", "from": "t@example.org",
	}); err != nil {
		t.Fatal(err)
	}

	// Two pools sharing a database, as two replicas do.
	replicaA, replicaB := NewPool(nil, accounts), NewPool(nil, accounts)
	replicaA.SetContactStore(db)
	replicaB.SetContactStore(db)

	// B builds its gateway FIRST, so it has a keyring with no contacts -- the
	// state that made this bug invisible.
	if gw := replicaB.ForTenant(ctx, "t_cust"); gw == nil {
		t.Fatal("replica B has no gateway")
	}

	key := generateTestKey(t, "alice@example.com")
	if err := replicaA.PersistContact(ctx, "t_cust", "alice@example.com", key); err != nil {
		t.Fatal(err)
	}

	// The announce reaches B through the bus in production; here the reload is
	// delivered directly, which is what the subscription handler does.
	replicaB.cache.Forget("t_cust")

	gwB := replicaB.ForTenant(ctx, "t_cust")
	if gwB == nil || gwB.KeyRing.GetContact("alice@example.com") == nil {
		t.Fatal("a key added on one replica never reached the other: whether an " +
			"alert is encrypted would depend on which pod served it")
	}
}

// Removing a contact removes it from the database too, or it comes back at the
// next restart.
func TestRemovingAContactRemovesTheStoredKey(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	db := newContactDB(t)

	if _, err := accounts.Set(ctx, "t_cust", integrations.ProviderEmail, map[string]string{
		"webhook_secret": "s", "smtp_host": "localhost:2525", "from": "t@example.org",
	}); err != nil {
		t.Fatal(err)
	}
	p := NewPool(nil, accounts)
	p.SetContactStore(db)

	key := generateTestKey(t, "alice@example.com")
	if err := p.PersistContact(ctx, "t_cust", "alice@example.com", key); err != nil {
		t.Fatal(err)
	}
	if err := p.ForgetContact(ctx, "t_cust", "alice@example.com"); err != nil {
		t.Fatal(err)
	}

	fresh := NewPool(nil, accounts)
	fresh.SetContactStore(db)
	gw := fresh.ForTenant(ctx, "t_cust")
	if gw == nil {
		t.Fatal("no gateway")
	}
	if gw.KeyRing.GetContact("alice@example.com") != nil {
		t.Fatal("a removed contact came back after a restart: the key was deleted " +
			"from the keyring but not from the database")
	}
}

// Two tenants' stored keys for the same address stay apart, in the DATABASE as
// well as in memory. The table's primary key is (tenant_id, email) for exactly
// this reason; keyed on email alone it would be the in-memory defect again, made
// durable.
func TestStoredContactsForTheSameAddressStayPerTenant(t *testing.T) {
	accounts := newAccounts(t)
	ctx := context.Background()
	db := newContactDB(t)

	for _, tid := range []string{"t_one", "t_two"} {
		if _, err := accounts.Set(ctx, tid, integrations.ProviderEmail, map[string]string{
			"webhook_secret": "s-" + tid, "smtp_host": "localhost:2525", "from": tid + "@example.org",
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPool(nil, accounts)
	p.SetContactStore(db)

	keyOne, keyTwo := generateTestKey(t, "alice@example.com"), generateTestKey(t, "alice@example.com")
	if err := p.PersistContact(ctx, "t_one", "alice@example.com", keyOne); err != nil {
		t.Fatal(err)
	}
	if err := p.PersistContact(ctx, "t_two", "alice@example.com", keyTwo); err != nil {
		t.Fatal(err)
	}

	rowsOne, err := db.ListEmailContacts(ctx, "t_one")
	if err != nil {
		t.Fatal(err)
	}
	rowsTwo, err := db.ListEmailContacts(ctx, "t_two")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsOne) != 1 || len(rowsTwo) != 1 {
		t.Fatalf("stored %d / %d contacts, want 1 each", len(rowsOne), len(rowsTwo))
	}
	if rowsOne[0].ArmoredKey == rowsTwo[0].ArmoredKey {
		t.Fatal("one tenant's stored key for an address overwrote the other's: " +
			"the in-memory collision made durable")
	}
}
