package middleware

import (
	"net/http"
	"strings"
)

// MaxBodySize returns middleware that limits the request body size for all
// non-webhook routes. Webhook handlers apply their own MaxBytesReader with
// transport-specific limits (e.g., 1MB for SBD, 100KB for Astrocast).
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip webhook paths — they apply their own body limits.
			if strings.HasPrefix(r.URL.Path, "/api/webhook/") {
				next.ServeHTTP(w, r)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}
