package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
)

// statusRecorder wraps http.ResponseWriter to capture status code and bytes written.
type statusRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.code = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	n, err := sr.ResponseWriter.Write(b)
	sr.bytes += n
	return n, err
}

// Logging is a middleware that logs HTTP request/response details.
// It uses correlation ID from context (set by RequestID middleware) and
// auth user/tenant from context (set by auth middleware).
// Skips /healthz, /readyz, and /metrics to avoid log noise.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy internal paths.
		p := r.URL.Path
		if p == "/healthz" || p == "/readyz" || p == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		// Inbound provider webhooks carry the tenant's secret in the last path
		// segment (MESHSAT-975), so the raw path must never be logged: these
		// lines go to stdout, the cluster's log store and anyone reading it.
		p = redactWebhookSecret(p)

		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sr, r)
		elapsed := time.Since(start)

		// Extract context values populated by earlier middleware.
		reqID := RequestIDFromContext(r.Context())
		tenantID := hubauth.TenantIDFromContext(r.Context())
		userID := ""
		if u := hubauth.FromContext(r.Context()); u != nil {
			userID = u.ID
		}

		attrs := []any{
			"method", r.Method,
			"path", p,
			"status", sr.code,
			"duration_ms", elapsed.Milliseconds(),
			"bytes", sr.bytes,
			"ip", r.RemoteAddr,
			"request_id", reqID,
		}
		if userID != "" {
			attrs = append(attrs, "user", userID)
		}
		if tenantID != "" && tenantID != "default" {
			attrs = append(attrs, "tenant", tenantID)
		}

		switch {
		case sr.code >= 500:
			slog.Warn("http request", attrs...)
		case sr.code >= 400:
			slog.Info("http request", attrs...)
		default:
			slog.Debug("http request", attrs...)
		}
	})
}

// webhookPrefix is the only route family whose path carries a secret.
const webhookPrefix = "/api/webhook/"

// redactWebhookSecret replaces the per-tenant secret in an inbound webhook path
// with a placeholder, leaving the provider visible so the logs stay useful:
// /api/webhook/cloudloop/9f3c… becomes /api/webhook/cloudloop/{secret}.
// Paths with no secret segment are returned unchanged.
func redactWebhookSecret(p string) string {
	if !strings.HasPrefix(p, webhookPrefix) {
		return p
	}
	rest := strings.TrimPrefix(p, webhookPrefix)
	i := strings.IndexByte(rest, '/')
	if i < 0 || i == len(rest)-1 {
		return p // /api/webhook/cloudloop, or a trailing slash: no secret present
	}
	return webhookPrefix + rest[:i] + "/{secret}"
}
