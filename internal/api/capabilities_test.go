package api

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func newCapSvc(t *testing.T) (*integrations.Service, context.Context) {
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
	svc := integrations.New(db, key)
	// The SSRF guard added in MESHSAT-1121 refuses a URL it cannot resolve to a
	// public address, which is every hostname in a test. It is doing its job;
	// this test is about capability resolution, not about the guard.
	svc.DisableURLCheckForTest()
	return svc, ctx
}

// Every provider named in the table must be a provider that exists. A typo here
// would report a feature as permanently unconfigurable and the customer would be
// sent to an Integrations page with nothing on it to fill in.
func TestEveryCapabilityNamesARealProvider(t *testing.T) {
	known := map[string]bool{}
	for _, s := range integrations.Specs {
		known[s.Provider] = true
	}
	for _, c := range featureCapabilities {
		if len(c.Providers) == 0 {
			t.Errorf("feature %q names no provider, so it can never resolve as configured", c.Feature)
		}
		for _, p := range c.Providers {
			if !known[p] {
				t.Errorf("feature %q names provider %q, which is not in integrations.Specs", c.Feature, p)
			}
		}
	}
}

// An unconfigured feature must say what will silently not happen. Without the
// sentence the UI can only say "unavailable", which reads as a fault in the Hub
// rather than as something the customer can fix -- and the whole defect this
// endpoint exists for was a customer not being told.
func TestAnUnconfiguredFeatureSaysWhatWillNotHappen(t *testing.T) {
	for _, c := range featureCapabilities {
		if strings.TrimSpace(c.Reason) == "" {
			t.Errorf("feature %q has no reason, so an unconfigured tenant is told nothing", c.Feature)
		}
		if c.Label == "" {
			t.Errorf("feature %q has no label", c.Feature)
		}
	}
}

// The three states a tenant can be in, and the one that must never happen.
func TestCapabilityResolvesPerTenant(t *testing.T) {
	svc, ctx := newCapSvc(t)
	h := NewCapabilitiesHandler(svc)

	notif := Capability{Feature: "notifications", Providers: []string{integrations.ProviderApprise, integrations.ProviderNtfy}, Reason: "no relay"}

	// 1. A tenant with nothing is not configured, and keeps its explanation.
	got, err := h.resolve(ctx, "t-nothing", notif)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Configured {
		t.Error("a tenant with no account resolved as configured")
	}
	if got.Reason == "" {
		t.Error("an unconfigured tenant lost its reason, so the UI has nothing to show")
	}

	// 2. A tenant with its OWN account is configured, and not as the platform's.
	if _, err := svc.Set(ctx, "t-own", integrations.ProviderNtfy, map[string]string{"url": "https://ntfy.example.com"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = h.resolve(ctx, "t-own", notif)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Configured {
		t.Error("a tenant with its own ntfy account resolved as unconfigured")
	}
	if got.Platform {
		t.Error("a tenant's own account was reported as the platform's")
	}
	if got.Reason != "" {
		t.Error("a configured feature still carries a reason to show the customer")
	}

	// 3. THE ONE THAT MUST NEVER HAPPEN. The platform account is the operator's
	// own environment configuration. If it resolved for an arbitrary tenant, the
	// UI would tell a customer that notifications work -- and they would be
	// delivered through the OPERATOR's relay, on the operator's credentials. The
	// guarantee comes from integrations.ForTenant returning nil for any
	// non-default tenant with no row; this asserts the capability layer does not
	// undo it.
	svc.SetPlatform(integrations.ProviderApprise, map[string]string{"url": "http://apprise:8000"})
	got, err = h.resolve(ctx, "t-stranger", notif)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Configured {
		t.Fatal("LEAK: a tenant with no account of its own was told the feature is configured, " +
			"which means it resolved the PLATFORM's relay and would deliver on the operator's credentials")
	}

	// The default tenant is the one the environment configuration belongs to,
	// and it should see it -- flagged as the platform's, not as its own.
	got, err = h.resolve(ctx, store.DefaultTenantID, notif)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Configured || !got.Platform {
		t.Errorf("the default tenant should see the platform account: configured=%v platform=%v", got.Configured, got.Platform)
	}
}

// A lookup failure must not be reported as "unconfigured".
//
// Doing so would tell a customer to go and set up something they already have,
// on the strength of a transient database error. The handler returns 500 and the
// SPA leaves the page as it was; this pins the behaviour of the layer under it.
func TestALookupFailureIsNotReportedAsUnconfigured(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	svc := integrations.New(db, key)
	svc.DisableURLCheckForTest()
	h := NewCapabilitiesHandler(svc)

	if _, err := svc.Set(ctx, "t1", integrations.ProviderHawkbit, map[string]string{
		"url": "https://hawkbit.example.com", "username": "u", "password": "p",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Break the store underneath, and drop the cached account so the lookup has
	// to reach it. A cache hit would prove nothing.
	svc.Invalidate("t1")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := h.resolve(ctx, "t1", Capability{
		Feature: "ota", Providers: []string{integrations.ProviderHawkbit}, Reason: "no hawkBit",
	})
	if err != nil {
		return // surfaced as a failure, which is what the handler turns into a 500
	}
	if !got.Configured {
		t.Fatal("a broken lookup was reported as 'not configured', so a customer who HAS " +
			"configured hawkBit would be told to go and configure it")
	}
}

// No view may decide whether a feature exists by pattern-matching an error
// message.
//
// This is the exact bug the endpoint replaces: OtaView and EmailView tested
// error text against /not found|404/i, so a caller who got 403 (not permitted)
// fell through as "available" and was shown an empty table -- told there is no
// data when the truth was they were not allowed to look. Any new page that
// copies the idiom fails here.
func TestNoViewDecidesAvailabilityByRegexingAnErrorMessage(t *testing.T) {
	views, err := filepath.Glob("../../web/src/views/*.vue")
	if err != nil || len(views) == 0 {
		t.Skipf("views not readable from here: %v", err)
	}
	// `unavailable = /.../.test(...)` in any spelling, across one line.
	bad := regexp.MustCompile(`unavailable[^\n]*=[^\n]*/[^\n]*\.test\(`)
	for _, v := range views {
		b, err := os.ReadFile(v) //nolint:gosec // test-only read of repo sources
		if err != nil {
			t.Fatalf("read %s: %v", v, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d decides availability by matching an error string:\n\t%s\n"+
					"Use the capabilities store instead: a 403 does not match this pattern, so a caller "+
					"who is not permitted is shown an empty page instead of being told why.",
					filepath.Base(v), i+1, strings.TrimSpace(line))
			}
		}
	}
}
