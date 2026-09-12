package auth

import "testing"

// The exemption list is what stands between the internet and everything that
// is not an API route. It used to be `!strings.HasPrefix(path, "/api/")`, which
// exempted /metrics, /startupz, /debug/pprof/* and the basemap along with the
// SPA -- invisible behind an IP allowlist, load-bearing without one.
func TestIsExempt(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
		why  string
	}{
		// Public by design.
		{"/", true, "SPA entry point"},
		{"/devices", true, "client-side route"},
		{"/fleet/bridge-1", true, "nested client-side route"},
		{"/assets/index-abc123.js", true, "SPA bundle"},
		{"/basemap/basemap.pmtiles", true, "self-hosted tiles"},
		{"/healthz", true, "liveness probe"},
		{"/readyz", true, "readiness probe"},
		{"/startupz", true, "startup probe"},
		{"/metrics", true, "guarded by its own bearer token, not the auth chain"},
		{"/favicon.ico", true, "browser default request"},

		// Webhooks: the provider authenticates itself.
		{"/api/webhook/rockblock", true, "provider signature"},
		{"/api/webhook/stripe/s3cr3t", true, "path secret plus a signed body"},

		// Unauthenticated auth endpoints.
		{"/api/auth/login", true, ""},
		{"/api/auth/config", true, ""},
		{"/api/auth/oidc/callback", true, ""},
		{"/api/bridges/b1/provision/nonce123", true, "the nonce is the auth"},
		{"/api/tak/enroll/0123456789abcdef0123456789abcdef/fedcba9876543210fedcba9876543210",
			true, "a TAK client on a phone has no account; the nonce is the auth"},

		// MUST NOT be exempt.
		{"/debug/pprof/", false, "heap dumps carry tokens and message plaintext"},
		{"/debug/pprof/heap", false, "no extension, must not read as an SPA route"},
		{"/debug/pprof/profile", false, ""},
		{"/api/devices", false, ""},
		{"/api/tenant/usage", false, ""},
		{"/api/admin/tenants/", false, ""},
		{"/api/ws", false, "authenticated, and now tenant-scoped"},
		{"/api/backup/export", false, ""},
		{"/api/bridges/b1/credentials", false, "not the provision claim shape"},
		{"/api/auth/keys", false, "owner-only in the router, never exempt here"},
		{"/api/tenant/tak", false, "the customer-facing TAK surface is owner/viewer, never public"},
		{"/api/tenant/tak/users", false, "listing who can see the fleet is not public"},
		{"/api/tak/enroll", false, "not the claim shape"},
		{"/api/tak/enroll/only-one-segment", false, "not the claim shape"},
	} {
		if got := isExempt(tc.path); got != tc.want {
			verb := "is exempt but must not be"
			if tc.want {
				verb = "is not exempt but must be"
			}
			t.Errorf("%s %s (%s)", tc.path, verb, tc.why)
		}
	}
}

// The TAK enrolment claim has to be reachable by a phone with no Hub account, so
// it is exempt -- and that exemption is the only thing standing in front of a
// certificate that can read a tenant's whole map. Too narrow and enrolment cannot
// work at all; too broad and something under /api/tak/ is public by accident.
func TestTheTAKEnrolmentClaimIsExemptAndNothingElseIs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nonce := "fedcba9876543210fedcba9876543210"
	if !isExempt("/api/tak/enroll/" + id + "/" + nonce) {
		t.Error("the claim path is not exempt; no phone could ever enrol")
	}
	// Anything that is not exactly the two-segment claim shape stays behind auth.
	for _, p := range []string{
		"/api/tak/enroll/" + id,
		"/api/tak/enroll/" + id + "/" + nonce + "/extra",
		"/api/tak/enroll/" + id + "/" + nonce + "/../../admin",
		"/api/tak/",
		"/api/tak/missions",
		"/api/tak/fleet-status",
	} {
		if isExempt(p) {
			t.Errorf("%s is exempt and must not be", p)
		}
	}
}

// The public donate link is followed by strangers from meshsat.net. If it ever
// stops being exempt they get a sign-in page instead of a payment page, which
// looks like the button is broken.
func TestThePublicDonateLinkIsExempt(t *testing.T) {
	for _, p := range []string{
		"/donate",
		"/api/donate",
		// Where Stripe returns them afterwards. These carry no session and
		// never will: the whole point is that a giver needs no account, and
		// the first real donation ended on a sign-in wall because the return
		// went into the SPA instead.
		"/donate/thanks",
		"/donate/cancelled",
	} {
		if !isExempt(p) {
			t.Errorf("%s is not exempt; a giver with no account cannot sign in", p)
		}
	}
	// And the neighbouring tenant route must NOT be: a donation attached to an
	// account is an owner action.
	if isExempt("/api/tenant/billing/donate") {
		t.Error("the signed-in donate route must stay behind auth")
	}
}
