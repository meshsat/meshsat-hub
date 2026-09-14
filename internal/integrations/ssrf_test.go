package integrations

import (
	"strings"
	"testing"
)

// A tenant chooses these URLs and the Hub then makes requests to them, which is
// a request-forgery primitive: the Hub sits in a cluster with a database, a
// broker, an object store and a cloud metadata service all reachable by name or
// by RFC1918 address.
//
// gosec found this as a G704 taint from an integrations account into
// wireguard's http.Do, and it was a REAL finding rather than a false positive:
// the value used to come from an operator-set environment variable and started
// coming from whatever a customer typed (MESHSAT-1121).
func TestATenantCannotPointAProviderAtInternalInfrastructure(t *testing.T) {
	svc, ctx := newSvc(t)

	for _, tc := range []struct {
		name, url string
	}{
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8080"},
		{"localhost by name", "http://localhost:51821"},
		{"the cluster's own database", "http://meshsat-hub-main-rw.meshsat-hub-db.svc:5432"},
		{"a private address", "http://10.0.0.5:8080"},
		{"the platform's own wg-easy", "http://wg-easy:51821"},
		{"a non-http scheme", "file:///etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Set(ctx, "t_cust", ProviderWireGuard, map[string]string{
				"url": tc.url, "password": "x",
			})
			if err == nil {
				t.Fatalf("a tenant stored %q as its wg-easy URL: the Hub would then "+
					"make requests to it with its own network identity", tc.url)
			}
		})
	}
}

// The guard must not refuse a legitimate external endpoint, or the feature is
// unusable and somebody removes the guard.
func TestALegitimateExternalURLIsAccepted(t *testing.T) {
	svc, ctx := newSvc(t)
	if _, err := svc.Set(ctx, "t_cust", ProviderNtfy, map[string]string{
		"url": "https://ntfy.sh",
	}); err != nil {
		t.Fatalf("a public ntfy server was refused: %v", err)
	}
}

// The PLATFORM's own in-cluster URLs are deliberately not checked. The operator
// chose them; http://wg-easy:51821 is correct for the platform and is exactly
// what the tenant path refuses.
func TestThePlatformsOwnInClusterURLIsNotRefused(t *testing.T) {
	svc, _ := newSvc(t)
	svc.SetPlatform(ProviderWireGuard, map[string]string{
		"url": "http://wg-easy:51821", "password": "x",
	})
	acct := svc.Platform(ProviderWireGuard)
	if acct == nil {
		t.Fatal("the operator's own in-cluster wg-easy URL was rejected: the " +
			"platform cannot reach its own services")
	}
	if got := acct.Get("url"); !strings.Contains(got, "wg-easy") {
		t.Errorf("platform url = %q", got)
	}
}
