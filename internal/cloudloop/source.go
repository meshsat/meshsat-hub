package cloudloop

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// Watching where satellite webhooks come from, without ever refusing one.
//
// HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS is "*" and the per-tenant path secret is
// what authenticates the caller. That is not laziness: Cloudloop publishes no
// webhook egress addresses anywhere -- its docs, the Ground Control
// knowledgebase and the security guide were all checked -- so there is no list
// to allowlist. RockBLOCK's published pair is Linode UK, different
// infrastructure, and does not transfer.
//
// It is tempting to narrow it to AWS eu-west-2 and call that hardening. Do not.
// This is the inbound path for satellite MO messages, which is the path an SOS
// arrives on. A provider that adds a region, moves a NAT or fails over would be
// silently refused, and the failure mode of a wrong allowlist here is a distress
// message that never arrives. The SOS invariant outranks the tidiness of a
// narrower CIDR.
//
// So: observe instead of block. A configured expected range does not gate
// anything -- every request is still processed -- but a request from outside it
// increments a counter and says so, which is what would tell you the path
// secret had leaked. Detection with no ability to drop traffic is the only
// shape this control may safely take.

// expectedSources is a parsed observe-only list. Entries may be single
// addresses or CIDRs.
type expectedSources struct {
	prefixes []netip.Prefix
	addrs    []netip.Addr
}

func parseExpectedSources(spec []string) expectedSources {
	var out expectedSources
	for _, raw := range spec {
		s := strings.TrimSpace(raw)
		if s == "" || s == "*" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out.prefixes = append(out.prefixes, p)
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			out.addrs = append(out.addrs, a)
			continue
		}
		slog.Warn("cloudloop: ignoring an unparseable expected webhook source", "value", s)
	}
	return out
}

func (e expectedSources) configured() bool { return len(e.prefixes) > 0 || len(e.addrs) > 0 }

func (e expectedSources) contains(ip string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, x := range e.addrs {
		if x.Unmap() == a {
			return true
		}
	}
	for _, p := range e.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// sourceIP resolves the caller, trusting X-Forwarded-For only from a proxy we
// already trust -- the same rule the blocking allowlist uses, because a header
// anyone may set is not evidence of anything.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" && isTrustedProxy(host) {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return host
}

// noteSource records that a webhook arrived, and whether from where it was
// expected. It NEVER returns a decision: callers must not branch on it.
func (h *WebhookHandler) noteSource(r *http.Request, provider string) {
	if !h.expected.configured() {
		return
	}
	ip := sourceIP(r)
	if h.expected.contains(ip) {
		return
	}
	metrics.WebhookUnexpectedSourceTotal.WithLabelValues(provider).Inc()
	// The path is deliberately not logged: its last segment is the tenant's
	// webhook secret, and a log line is exactly where a URL travels to.
	slog.Warn("webhook: a delivery arrived from outside the expected source range. "+
		"The request was PROCESSED -- this is an observation, not a refusal, because "+
		"refusing here could drop a distress message. If this is not the provider "+
		"changing address, the path secret may have leaked: rotate it.",
		"provider", provider, "source", ip)
}
