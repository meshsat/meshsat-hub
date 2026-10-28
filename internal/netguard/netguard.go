package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Package netguard refuses a URL that would make the Hub fetch something on its
// own side of the network.
//
// Extracted from internal/webhook in MESHSAT-1121, unchanged, because the same
// primitive appeared somewhere new. Outbound webhooks were the first place a
// CUSTOMER could choose a URL the Hub then fetches; per-tenant provider accounts
// made that true of APRS-IS, Apprise, ntfy, the email gateway, wg-easy and
// hawkBit as well. gosec found it first, as a G704 taint from an integrations
// account into wireguard's http.Do -- a real finding, not a false positive: the
// value used to come from an operator-set environment variable and now comes
// from whatever a customer typed.
//
// The operator's OWN configuration is deliberately NOT checked against this.
// http://wg-easy:51821 is exactly the kind of in-cluster name this refuses, and
// it is correct for the platform to use it: the difference is who chose it.
//
// A self-hosted Hub serving one organisation on its own network is the other
// case where a customer-typed address is the operator's own (MESHSAT-1736): the
// Matrix server, the Apprise relay or the TAK server stand next to the Hub on
// 192.168.x.x. AllowPrivateTargets lifts the private-address refusal for that
// Hub, by the operator's setting; the hosted Hub never sets it.

// allowPrivate is the operator's say that this Hub may reach its own network.
var allowPrivate atomic.Bool

// AllowPrivateTargets lets a configured service live on a private network: a
// self-hosted Hub whose dispatchers, Matrix server and Hub share one LAN
// (HUB_ALLOW_PRIVATE_TARGETS). Loopback and internal names are allowed too;
// link-local (the cloud metadata service), multicast and the unspecified
// address stay refused, nothing a service is reached on. Set once at boot.
func AllowPrivateTargets(on bool) { allowPrivate.Store(on) }

// PrivateTargetsAllowed reports the setting.
func PrivateTargetsAllowed() bool { return allowPrivate.Load() }

// ErrUnsafe is returned for a URL that points back into infrastructure rather
// than out at somebody's own endpoint.
var ErrUnsafe = errors.New("unsafe target")

// ValidatePublicURL refuses a URL that resolves to an address the Hub keeps on
// its own side of the wire.
//
// The Hub sits inside a Kubernetes cluster with an object store, a database, a
// broker and a cloud metadata service all reachable by name or by RFC1918
// address. An unvalidated outbound URL is a request forgery primitive: give it
// http://169.254.169.254/... or http://meshsat-hub-main-rw.meshsat-hub-db.svc
// and the Hub fetches it with its own network identity.
//
// Check at REGISTRATION and again at USE where that is practical. A name that
// resolves publicly when it is saved can resolve to 127.0.0.1 an hour later, and
// a check that runs only at registration does not see that.
func ValidatePublicURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrUnsafe)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not allowed, use http or https", ErrUnsafe, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: no host", ErrUnsafe)
	}
	return validateHost(host)
}

// ValidatePublicHost is ValidatePublicURL for a setting that is a bare host
// rather than a URL: the address of a tenant's own TAK server, which the Hub
// dials as raw TLS and which therefore never had a scheme to check
// (MESHSAT-1460).
//
// It takes a hostname or an address literal WITHOUT a port. Anything that looks
// like more than that is refused rather than interpreted, because a value the
// guard parses one way and the dialer another is how a check is walked around.
func ValidatePublicHost(raw string) error {
	host := strings.TrimSpace(raw)
	if host == "" {
		return fmt.Errorf("%w: no host", ErrUnsafe)
	}
	if strings.ContainsAny(host, "/@?#\\ \t\r\n") {
		return fmt.Errorf("%w: %q is not a bare host name or address", ErrUnsafe, host)
	}
	// An IPv6 literal may arrive bracketed, the way it is written beside a port.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if ip := net.ParseIP(host); ip == nil && strings.Contains(host, ":") {
		// Not an address, and a name has no colon: this is host:port, which the
		// form asks for in two fields.
		return fmt.Errorf("%w: %q carries a port, give the host alone", ErrUnsafe, host)
	}
	return validateHost(host)
}

// validateHost is the half ValidatePublicURL and ValidatePublicHost share: the
// name must not be an internal one, and everything it resolves to must be public.
func validateHost(host string) error {
	// A bare ".svc" or "localhost" name never needs resolving to be wrong.
	lower := strings.ToLower(host)
	if !allowPrivate.Load() && (lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".svc") || strings.HasSuffix(lower, ".svc.cluster.local") ||
		strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local")) {
		return fmt.Errorf("%w: %s is an internal name", ErrUnsafe, host)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		// Fail closed: a name we cannot resolve is a name we cannot vouch for.
		return fmt.Errorf("%w: cannot resolve %s", ErrUnsafe, host)
	}
	for _, ip := range ips {
		if !reachable(ip) {
			return fmt.Errorf("%w: %s resolves to %s, which is not a public address", ErrUnsafe, host, ip)
		}
	}
	return nil
}

// reachable is IsPublicIP, or with AllowPrivateTargets on, anything an
// operator's own service could be listening on.
func reachable(ip net.IP) bool {
	if !allowPrivate.Load() {
		return IsPublicIP(ip)
	}
	return ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsInterfaceLocalMulticast() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// IsPublicIP reports whether an address is one the Hub may legitimately call
// out to. Everything the runtime keeps on its own side of the wire is refused.
// It never looks at AllowPrivateTargets: the guards do, through reachable.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return false
	}
	// 169.254.169.254 is caught by IsLinkLocalUnicast above; these are the
	// ranges Go does not classify for us.
	for _, cidr := range []string{
		"100.64.0.0/10", // carrier-grade NAT, and the cluster's own mesh
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
		"::/128",        // unspecified
		"64:ff9b::/96",  // NAT64
		"2002::/16",     // 6to4
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err == nil && n.Contains(ip) {
			return false
		}
	}
	return true
}

// SafeHTTPClient returns an http.Client that refuses to CONNECT to an address
// the Hub keeps on its own side of the wire, checked at dial time.
//
// This is the half ValidatePublicURL cannot do. That one resolves the name when
// the URL is SAVED, and a name that resolves publicly then can resolve to
// 127.0.0.1 an hour later -- DNS rebinding, which is the standard way past a
// registration-time check. The dialer's Control hook runs after resolution and
// immediately before connect, on the address actually being dialled, so there is
// no window between the check and the use.
//
// Both are wanted. The save-time check gives the customer an error while they
// are looking at the form; this one is what actually holds (MESHSAT-1121).
func SafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   DialControl,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// DialControl is a net.Dialer Control hook that refuses to CONNECT to an address
// the Hub keeps on its own side of the wire.
//
// It runs after resolution and immediately before connect, on the address
// actually being dialled, so there is no window between the check and the use.
// SafeHTTPClient is this on an http.Transport; it is exported so a dialer that is
// not HTTP -- the TLS dial to a tenant's own TAK server -- gets the same guard
// instead of a second implementation of it (MESHSAT-1460).
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse %q", ErrUnsafe, address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control is called with a resolved literal; anything else is
		// unexpected, so refuse rather than guess.
		return fmt.Errorf("%w: %q is not an IP", ErrUnsafe, host)
	}
	if !reachable(ip) {
		return fmt.Errorf("%w: refusing to connect to %s", ErrUnsafe, ip)
	}
	return nil
}
