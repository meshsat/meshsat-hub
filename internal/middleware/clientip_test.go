package middleware

import (
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// resetTrusted lets each case configure its own proxy set.
func resetTrusted(t *testing.T, cidrs string) {
	t.Helper()
	t.Setenv("HUB_TRUSTED_PROXIES", cidrs)
	trustedOnce = sync.Once{}
	trustedNets = nil
	t.Cleanup(func() {
		trustedOnce = sync.Once{}
		trustedNets = nil
	})
}

// A forwarding header from a peer we do not trust is the peer's own invention
// and must not become the rate-limit key -- that is what let an attacker mint a
// fresh login budget per request.
func TestClientIP_IgnoresForwardedFromUntrustedPeer(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "203.0.113.7:44321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want the direct peer 203.0.113.7", got)
	}
}

// From a trusted proxy, the rightmost entry that is not ours is the client.
func TestClientIP_TakesTheClientFromATrustedChain(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.2.5.101:9999" // ingress pod
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 10.2.0.4")
	if got := ClientIP(r); got != "198.51.100.9" {
		t.Errorf("ClientIP = %q, want 198.51.100.9", got)
	}
}

// A client that pads the header with junk before the real hop cannot displace
// the rightmost untrusted entry.
func TestClientIP_SpoofedPrefixDoesNotWin(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.2.5.101:9999"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.9, 10.2.0.4")
	if got := ClientIP(r); got != "198.51.100.9" {
		t.Errorf("ClientIP = %q, want the last untrusted hop 198.51.100.9", got)
	}
}

// Unconfigured means trust nothing: over-group rather than let a header forge
// the key. Wrong-but-safe beats wrong-and-bypassable.
func TestClientIP_NoTrustedProxiesIgnoresTheHeader(t *testing.T) {
	resetTrusted(t, "")
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.2.5.101:9999"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r); got != "10.2.5.101" {
		t.Errorf("ClientIP = %q, want 10.2.5.101", got)
	}
}

func TestClientIP_IPv6AndBareAddresses(t *testing.T) {
	resetTrusted(t, "2a0c:9a40::/32")
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2a0c:9a40:8e20::7]:443"
	r.Header.Set("X-Forwarded-For", "2001:db8::1")
	if got := ClientIP(r); got != "2001:db8::1" {
		t.Errorf("ClientIP = %q, want 2001:db8::1", got)
	}
	_ = os.Getenv("HUB_TRUSTED_PROXIES")
}
