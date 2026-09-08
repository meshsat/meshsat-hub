package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// MetricsTokenGuard wraps /metrics: when token is set, the scrape must carry
// it as a bearer (Prometheus `authorization.credentials`); the endpoint stays
// auth-exempt otherwise, as before.
func MetricsTokenGuard(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
