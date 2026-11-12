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
		{"/og-image.jpg", true, "index.html's og:image, fetched by link previews (MESHSAT-1539)"},
		{"/apple-touch-icon.png", true, "index.html's home-screen icon (MESHSAT-1539)"},
		{"/logo.svg", false, "in dist but referenced by nothing; exempt by name only"},
		{"/icons.svg", false, "in dist but referenced by nothing; exempt by name only"},

		// Webhooks: the provider authenticates itself.
		{"/api/webhook/rockblock", true, "provider signature"},
		{"/api/webhook/stripe/s3cr3t", true, "path secret plus a signed body"},

		// Unauthenticated auth endpoints.
		{"/api/auth/login", true, ""},
		{"/api/auth/config", true, ""},
		{"/api/auth/oidc/callback", true, ""},
		{"/api/bridges/b1/provision/0123456789abcdef0123456789abcdef", true, "the nonce is the auth"},
		{"/api/bridges/b1/provision/qr", false, "the owner asking for a QR is not a claim (MESHSAT-1238)"},
		{"/api/bridges/b1/provision/nonce123", false, "not a nonce the Hub issues"},
		{"/api/bridges/b1/provision/0123456789ABCDEF0123456789ABCDEF", false, "issued nonces are lowercase hex"},
		{"/api/bridges/b1/provision", false, "the owner's bundle request"},

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
		{"/api/tenant/tak/users", false, "gone with port 8089, and never public"},
		{"/api/tak/enroll", false, "gone with port 8089"},
		{"/api/tak/enroll/0123456789abcdef0123456789abcdef/fedcba9876543210fedcba9876543210",
			false, "the enrolment claim was exempt until MESHSAT-1466 retired phone enrolment"},
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

// The TAK enrolment claim was the one exempt path under /api/tak/ until phone
// enrolment was retired with public port 8089 (MESHSAT-1466). Nothing under it
// may be public now, the old claim shape included: a phone holding a link from
// before the closure meets authentication like any other caller.
func TestNothingUnderTheTAKAPIIsExempt(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nonce := "fedcba9876543210fedcba9876543210"
	for _, p := range []string{
		"/api/tak/enroll/" + id + "/" + nonce,
		"/api/tak/enroll/" + id,
		"/api/tak/enroll/" + id + "/" + nonce + "/extra",
		"/api/tak/",
		"/api/tak/contacts",
		"/api/tak/missions",
		"/api/tak/fleet-status",
	} {
		if isExempt(p) {
			t.Errorf("%s is exempt and must not be", p)
		}
	}
	// Positive control: the matcher still exempts something, so a blanket "false"
	// cannot pass this test.
	if !isExempt("/api/bridges/b1/provision/" + id) {
		t.Fatal("the bridge provision claim is no longer exempt; the control is broken")
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
