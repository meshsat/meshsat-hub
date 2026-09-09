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
		{"/api/webhook/kofi/s3cr3t", true, "path secret plus body token"},

		// Unauthenticated auth endpoints.
		{"/api/auth/login", true, ""},
		{"/api/auth/config", true, ""},
		{"/api/auth/oidc/callback", true, ""},
		{"/api/bridges/b1/provision/nonce123", true, "the nonce is the auth"},

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
