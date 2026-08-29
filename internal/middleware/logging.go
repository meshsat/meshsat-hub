package middleware

import (
	"log/slog"
	"net/http"
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
