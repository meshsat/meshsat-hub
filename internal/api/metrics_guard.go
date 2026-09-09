package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// MetricsTokenGuard wraps /metrics: the scrape must carry the token as a
// bearer (Prometheus `authorization.credentials`).
//
// This guard IS the authentication for /metrics -- the endpoint is exempt from
// the auth chain because the metrics token is neither a session nor an API key
// and the chain would reject the scrape. So an unset token FAILS CLOSED. It
// used to return the handler unwrapped, which meant a secret that failed to
// sync served the whole metric set to the internet with nothing behind it.
func MetricsTokenGuard(token string, next http.Handler) http.Handler {
	if token == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slog.Warn("metrics: HUB_METRICS_TOKEN is unset, refusing the scrape")
			http.Error(w, "metrics endpoint is not configured", http.StatusServiceUnavailable)
		})
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
