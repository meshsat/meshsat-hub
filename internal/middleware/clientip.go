package middleware

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// ClientIP returns the address a rate limiter should key on.
//
// Two wrong answers were in use before this existed, each wrong in the
// opposite direction.
//
// The webhook limiter keyed on r.RemoteAddr. Behind the VPS HAProxy and
// ingress-nginx that is the ingress pod, so every provider and every tenant
// shared one 60-per-minute bucket: one noisy provider 429s everybody's
// satellite ingest, and the limit protects nothing.
//
// The login limiter keyed on the first X-Forwarded-For value with no check on
// who set it. Anyone could mint a fresh attempt budget per request by varying a
// header, and the map it wrote to grew without bound off attacker-controlled
// keys.
//
// The correct answer needs to know which hops are ours. HUB_TRUSTED_PROXIES is
// a comma-separated list of CIDRs; the rightmost XFF entry that is NOT one of
// ours is the client. With nothing configured we fall back to RemoteAddr,
// which is wrong-but-safe: it over-groups rather than letting a header forge
// the key.
func ClientIP(r *http.Request) string {
	// Tor offers no client identity to key on. Every visitor arrives from the
	// same pod, and that pod is inside HUB_TRUSTED_PROXIES because the list
	// names the pod network -- so the branch below would read an
	// X-Forwarded-For the anonymous client wrote itself. That let an onion
	// client mint a fresh budget per request, defeating the login, auth,
	// webhook and relay limiters at once, and let it spend a NAMED VICTIM's
	// budget by sending that person's address (MESHSAT-1169).
	//
	// So the onion gets one key of its own. Tor users consequently share a
	// bucket, which is inherent rather than a shortcut: there is no per-client
	// identity on this path. A shared budget is a bounded failure; a forgeable
	// key is an unbounded one.
	if ChannelOf(r) == ChannelOnion {
		return OnionClientKey
	}

	remote := hostOnly(r.RemoteAddr)
	trusted := trustedProxies()
	if len(trusted) == 0 || !inAny(remote, trusted) {
		// The direct peer is not a proxy we trust, so it is the client and any
		// forwarding header it sent is its own invention.
		return remote
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remote
	}
	parts := strings.Split(fwd, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := hostOnly(strings.TrimSpace(parts[i]))
		if ip == "" || net.ParseIP(ip) == nil {
			continue
		}
		if !inAny(ip, trusted) {
			return ip
		}
	}
	return remote
}

// OnionClientKey is the rate-limit key every Tor request shares.
//
// It is deliberately not an address: it cannot collide with a real client's IP,
// and it is the honest thing for an audit entry to record for a Tor request --
// the tor pod's address would name our own infrastructure as the actor, and any
// forwarded address would be the client's own claim.
const OnionClientKey = "onion"

func hostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return strings.Trim(addr, "[]")
}

func inAny(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

var (
	trustedOnce sync.Once
	trustedNets []*net.IPNet
)

// trustedProxies parses HUB_TRUSTED_PROXIES once. Unset means "trust nothing",
// which makes ClientIP ignore forwarding headers entirely.
func trustedProxies() []*net.IPNet {
	trustedOnce.Do(func() {
		raw := os.Getenv("HUB_TRUSTED_PROXIES")
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.Contains(part, "/") {
				if strings.Contains(part, ":") {
					part += "/128"
				} else {
					part += "/32"
				}
			}
			if _, n, err := net.ParseCIDR(part); err == nil {
				trustedNets = append(trustedNets, n)
			}
		}
	})
	return trustedNets
}
