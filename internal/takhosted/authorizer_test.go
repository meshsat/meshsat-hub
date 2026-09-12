package takhosted

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// newAuthzStore gives each test its own migrated store with one active tenant.
func newAuthzStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.CreateTenant(ctx, &store.Tenant{
		ID: "t1", Slug: "t1", Name: "t1", Status: store.TenantActive,
	}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return db
}

func activeTenant(context.Context, string) (bool, error) { return true, nil }

func addUser(t *testing.T, s store.Store, u store.TAKUser) {
	t.Helper()
	if err := s.CreateTAKUser(context.Background(), "t1", &u); err != nil {
		t.Fatalf("create TAK user %s: %v", u.Username, err)
	}
}

// The four refusals, each distinguishable. takfront turns these into a refusal
// reason and an audit entry, so conflating them would make a tenant's audit log
// say "refused" without saying why.
func TestTheAuthorizerDistinguishesEveryRefusal(t *testing.T) {
	s := newAuthzStore(t)
	addUser(t, s, store.TAKUser{Username: "alice", Active: true, CertSerial: "111"})
	addUser(t, s, store.TAKUser{Username: "bob", Active: false, CertSerial: "222"})
	addUser(t, s, store.TAKUser{Username: "carol", Active: true, CertSerial: "333", RevokedSerial: "333"})

	a := NewAuthorizer(s, activeTenant).WithTTL(0)
	ctx := context.Background()

	if err := a.AllowTAKClient(ctx, "t1", "alice", big.NewInt(111)); err != nil {
		t.Errorf("an active user with a current certificate was refused: %v", err)
	}
	if err := a.AllowTAKClient(ctx, "t1", "nobody", big.NewInt(999)); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("unknown common name gave %v, want ErrNoSuchUser", err)
	}
	if err := a.AllowTAKClient(ctx, "t1", "bob", big.NewInt(222)); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated user gave %v, want ErrUserInactive", err)
	}
	if err := a.AllowTAKClient(ctx, "t1", "carol", big.NewInt(333)); !errors.Is(err, ErrCertRevoked) {
		t.Errorf("revoked serial gave %v, want ErrCertRevoked", err)
	}
	// Another tenant's user is simply unknown here, which is what keeps a
	// certificate from one tenant useless in another even if the CA were shared.
	if err := a.AllowTAKClient(ctx, "t-other", "alice", big.NewInt(111)); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("cross-tenant lookup gave %v, want ErrNoSuchUser", err)
	}
}

// A revoked SERIAL must not lock the person out: they are still a member, and a
// re-enrolled phone with a new certificate has to work immediately.
func TestRevokingOneCertificateLeavesTheAccountUsable(t *testing.T) {
	s := newAuthzStore(t)
	// The serials are the DECIMAL STRINGS a certificate's big.Int renders to,
	// because that is exactly what the authorizer compares against the stored
	// column. Storing a word like "old" here would compare a hash against a
	// name and the test would pass without proving anything.
	const revoked, current = "7001", "7002"
	addUser(t, s, store.TAKUser{
		Username: "dave", Active: true,
		CertSerial: current, RevokedSerial: revoked,
	})
	a := NewAuthorizer(s, activeTenant).WithTTL(0)
	ctx := context.Background()

	if err := a.AllowTAKClient(ctx, "t1", "dave", serialOf(t, revoked)); !errors.Is(err, ErrCertRevoked) {
		t.Errorf("the revoked certificate was accepted: %v", err)
	}
	// The replacement works immediately: the person is still a member, and a
	// re-enrolled phone must not wait for anything to expire.
	if err := a.AllowTAKClient(ctx, "t1", "dave", serialOf(t, current)); err != nil {
		t.Errorf("the replacement certificate was refused: %v", err)
	}
}

// A suspended tenant's phones stop, whatever their own accounts say.
func TestASuspendedTenantStopsServing(t *testing.T) {
	s := newAuthzStore(t)
	addUser(t, s, store.TAKUser{Username: "erin", Active: true, CertSerial: "1"})

	suspended := func(context.Context, string) (bool, error) { return false, nil }
	a := NewAuthorizer(s, suspended).WithTTL(0)
	if err := a.AllowTAKClient(context.Background(), "t1", "erin", serialOf(t, "1")); !errors.Is(err, ErrTenantInactive) {
		t.Errorf("a suspended tenant still served: %v", err)
	}
}

// The asymmetry worth a test of its own.
//
// internal/quota fails OPEN on a lookup error, deliberately: refusing to register
// a customer's kit because a count query timed out is the worse failure. This
// authorizer fails CLOSED, equally deliberately: admitting a possibly-revoked
// phone into a tenant's live map is the worse failure here. Same kind of error,
// opposite correct direction, and somebody will eventually try to "make them
// consistent".
func TestADatabaseFailureRefusesRatherThanAdmits(t *testing.T) {
	a := NewAuthorizer(brokenStore{newAuthzStore(t)}, activeTenant).WithTTL(0)
	err := a.AllowTAKClient(context.Background(), "t1", "alice", serialOf(t, "1"))
	if err == nil {
		t.Fatal("a store failure ADMITTED the connection; it must refuse. " +
			"internal/quota fails open on purpose, this must not.")
	}
	if isDecision(err) {
		t.Errorf("a store failure was classified as a settled decision (%v), so it would be cached", err)
	}
}

// A transient failure must not be cached, or one database blip pins a refusal for
// the whole TTL and a tenant's phones stay out long after the blip passed.
func TestATransientFailureIsNotCachedButADecisionIs(t *testing.T) {
	s := newAuthzStore(t)
	addUser(t, s, store.TAKUser{Username: "frank", Active: true, CertSerial: "1"})

	flaky := &flakyStore{Store: s, fail: true}
	a := NewAuthorizer(flaky, activeTenant)
	ctx := context.Background()

	if err := a.AllowTAKClient(ctx, "t1", "frank", serialOf(t, "1")); err == nil {
		t.Fatal("the first call should have failed")
	}
	// The failure healed; the very next call must ask again rather than serve a
	// cached refusal.
	flaky.fail = false
	if err := a.AllowTAKClient(ctx, "t1", "frank", serialOf(t, "1")); err != nil {
		t.Errorf("a healed store still refused, so the transient failure was cached: %v", err)
	}
	// And now the allow IS cached: breaking the store again must not change the
	// answer within the TTL.
	flaky.fail = true
	if err := a.AllowTAKClient(ctx, "t1", "frank", serialOf(t, "1")); err != nil {
		t.Errorf("the cached allow was not reused: %v", err)
	}
}

// Forget is what makes a revocation take effect now rather than at the end of the
// TTL. The API calls it whenever it changes a TAK user.
func TestForgetDropsTheCachedDecision(t *testing.T) {
	s := newAuthzStore(t)
	addUser(t, s, store.TAKUser{Username: "gina", Active: true, CertSerial: "1"})
	a := NewAuthorizer(s, activeTenant) // default TTL, so caching is on
	ctx := context.Background()

	if err := a.AllowTAKClient(ctx, "t1", "gina", serialOf(t, "1")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Deactivate behind the cache: without Forget the stale allow stands.
	gina, err := s.GetTAKUser(ctx, "t1", "gina")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gina.Active = false
	if err := s.UpdateTAKUser(ctx, "t1", gina); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := a.AllowTAKClient(ctx, "t1", "gina", serialOf(t, "1")); err != nil {
		t.Log("the cache is still serving the old allow, which is the behaviour Forget exists to fix")
	}
	a.Forget("t1", "gina")
	if err := a.AllowTAKClient(ctx, "t1", "gina", serialOf(t, "1")); !errors.Is(err, ErrUserInactive) {
		t.Errorf("after Forget the deactivation was still not seen: %v", err)
	}

	// ForgetTenant does the same for every user, for a suspension or a purge.
	a.ForgetTenant("t1")
	if err := a.AllowTAKClient(ctx, "t1", "gina", serialOf(t, "1")); !errors.Is(err, ErrUserInactive) {
		t.Errorf("after ForgetTenant: %v", err)
	}
}

// An empty tenant or common name is refused rather than used to query: a blank
// common name reaching the database would be a lookup nobody intended.
func TestBlankIdentityIsRefusedWithoutAQuery(t *testing.T) {
	a := NewAuthorizer(newAuthzStore(t), activeTenant).WithTTL(0)
	ctx := context.Background()
	for _, tc := range []struct{ tenant, cn string }{{"", "alice"}, {"t1", ""}, {"", ""}} {
		if err := a.AllowTAKClient(ctx, tc.tenant, tc.cn, serialOf(t, "1")); !errors.Is(err, ErrNoSuchUser) {
			t.Errorf("tenant=%q cn=%q gave %v, want ErrNoSuchUser", tc.tenant, tc.cn, err)
		}
	}
}

// serialOf turns a decimal serial string into the big.Int a certificate carries,
// which is what the authorizer stringifies and compares against the stored
// column. Every serial in these tests is decimal for that reason, so there is no
// fallback branch here: an unparseable serial is a mistake in the test, not a
// case to paper over.
func serialOf(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("serialOf(%q): not a decimal serial; the authorizer compares decimal strings", s)
	}
	return n
}

// brokenStore fails every TAK user read.
type brokenStore struct{ store.Store }

func (brokenStore) GetTAKUser(context.Context, string, string) (*store.TAKUser, error) {
	return nil, errors.New("database is unreachable")
}

// flakyStore fails on demand, so one test can watch a failure heal.
type flakyStore struct {
	store.Store
	fail bool
}

func (f *flakyStore) GetTAKUser(ctx context.Context, tenantID, username string) (*store.TAKUser, error) {
	if f.fail {
		return nil, errors.New("database is unreachable")
	}
	return f.Store.GetTAKUser(ctx, tenantID, username)
}
