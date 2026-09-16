package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// onionRequest is a request as it actually arrives off the hidden service: the
// peer is the tor pod, which is inside the trusted-proxy list because that list
// names the pod network, and the headers are whatever the anonymous client sent.
func onionRequest(t *testing.T, xff string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.2.2.247:39114" // tor-0
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	var got *http.Request
	WithChannel(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		got = rr
	}), ChannelOnion).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// The regression for MESHSAT-1169. An onion client sent its own X-Forwarded-For
// from a peer the Hub trusts, so it chose its own rate-limit key and could mint
// a fresh one per request -- defeating the login, auth, webhook and relay
// limiters together.
func TestOnionClientCannotChooseItsRateLimitKey(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")

	first := ClientIP(onionRequest(t, "1.2.3.4"))
	second := ClientIP(onionRequest(t, "5.6.7.8"))

	if first != OnionClientKey || second != OnionClientKey {
		t.Fatalf("onion requests keyed on %q and %q, want both %q",
			first, second, OnionClientKey)
	}
	if first != second {
		t.Errorf("two onion requests got different keys (%q, %q): the budget is still forgeable",
			first, second)
	}
}

// The other half of the same defect: naming a victim's address spent THAT
// person's login budget and wrote their address into the audit trail as fact.
func TestOnionClientCannotSpendAVictimsBudget(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")

	const victim = "198.51.100.9"
	if got := ClientIP(onionRequest(t, victim)); got == victim {
		t.Fatalf("an onion request was attributed to %s, which is a real client's address", victim)
	}
}

// The key must not be an address at all, or it could collide with a real
// client's and hand Tor traffic somebody else's budget.
func TestOnionKeyIsNotAnAddress(t *testing.T) {
	if OnionClientKey == "" {
		t.Fatal("OnionClientKey is empty")
	}
	if net.ParseIP(OnionClientKey) != nil {
		t.Errorf("OnionClientKey %q parses as an IP and could collide with a real client's budget",
			OnionClientKey)
	}
}

// Edge traffic keeps the behaviour it had: from a trusted proxy the forwarded
// client still wins, so this change costs the internet path nothing.
func TestInternetChannelStillReadsForwardedFor(t *testing.T) {
	resetTrusted(t, "10.2.0.0/16")

	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.2.5.101:9999" // ingress-nginx pod
	r.Header.Set("X-Forwarded-For", "198.51.100.9")

	if got := ClientIP(r); got != "198.51.100.9" {
		t.Errorf("ClientIP = %q, want 198.51.100.9", got)
	}
}

// Anything unmarked is edge traffic. The default has to be the channel whose
// forwarding headers our own proxies set, so that a listener added later cannot
// silently inherit the onion's exemptions.
func TestUnmarkedRequestsDefaultToTheInternetChannel(t *testing.T) {
	r := httptest.NewRequest("GET", "/healthz", nil)
	if got := ChannelOf(r); got != ChannelInternet {
		t.Errorf("ChannelOf(unmarked) = %q, want %q", got, ChannelInternet)
	}
}

func TestWithChannelMarksTheRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/healthz", nil)
	var seen Channel
	WithChannel(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		seen = ChannelOf(rr)
	}), ChannelOnion).ServeHTTP(httptest.NewRecorder(), r)

	if seen != ChannelOnion {
		t.Errorf("handler saw channel %q, want %q", seen, ChannelOnion)
	}
}
