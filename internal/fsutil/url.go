package fsutil

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateBaseURL checks an operator-configured integration base URL: it
// must parse, use http or https and carry a host. The trailing slash is
// trimmed. Integration clients call this once at construction so a
// misconfiguration is reported at startup rather than on the first request.
func ValidateBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid base url %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid base url %q: scheme must be http(s) with a host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid base url %q: credentials in url are not allowed", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}
