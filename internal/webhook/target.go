package webhook

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrUnsafeTarget is returned for a webhook URL that points back into
// infrastructure rather than out at a customer's endpoint.
var ErrUnsafeTarget = errors.New("webhook: unsafe target")

// ValidateTarget refuses an outbound webhook URL that would make the Hub fetch
// something on its own side of the network.
//
// The Hub sits inside a Kubernetes cluster with an object store, a database, a
// broker and a cloud metadata service all reachable by name or by RFC1918
// address. An unvalidated outbound webhook is a request forgery primitive:
// register http://169.254.169.254/... or http://meshsat-hub-main-rw.meshsat-hub-db.svc
// and the Hub fetches it with its own network identity and hands you the
// response status. Owner-only is the other half of this control; neither half
// is sufficient alone.
//
// Checked at registration AND at dispatch, deliberately. A name that resolves
// publicly when it is registered can resolve to 127.0.0.1 an hour later, and a
// check that runs only at registration does not see that.
func ValidateTarget(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrUnsafeTarget)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not allowed, use http or https", ErrUnsafeTarget, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: no host", ErrUnsafeTarget)
	}
	// A bare ".svc" or "localhost" name never needs resolving to be wrong.
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".svc") || strings.HasSuffix(lower, ".svc.cluster.local") ||
		strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local") {
		return fmt.Errorf("%w: %s is an internal name", ErrUnsafeTarget, host)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		// Fail closed: a name we cannot resolve is a name we cannot vouch for.
		return fmt.Errorf("%w: cannot resolve %s", ErrUnsafeTarget, host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s, which is not a public address", ErrUnsafeTarget, host, ip)
		}
	}
	return nil
}

// isPublicIP reports whether an address is one the Hub may legitimately call
// out to. Everything the runtime keeps on its own side of the wire is refused.
func isPublicIP(ip net.IP) bool {
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
