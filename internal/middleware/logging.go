package middleware

import (
	"bufio"
	"log/slog"
	"net"
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

// Hijack lets a WebSocket upgrade through the recorder; see the same method
// on metrics.statusWriter for why an embedded interface is not enough.
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		sr.code = http.StatusSwitchingProtocols
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap lets http.ResponseController reach the server's writer through the
// recorder (see metrics.statusWriter.Unwrap).
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// Flush keeps streaming responses working through the recorder.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
		p = redactClaimNonce(redactWebhookSecret(p))

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

// redactClaimNonce hides the bearer token in the two claim paths that carry
// one: a provisioning bundle, GET /api/bridges/{id}/provision/{nonce}, which
// hands out a bridge's MQTT password and client private key, and a TAK
// enrolment, GET /api/tak/enroll/{claimID}/{nonce}. The nonce IS the
// credential there. Once the provisioning claim could answer 503 and keep a
// bundle claimable (MESHSAT-1298), a logged claim path was a live token in the
// cluster's log store for up to 30 minutes. Only a segment shaped like a nonce
// (32 lowercase hex) is replaced, so /provision/qr and /provision/status stay
// readable, and a TAK claim keeps its claim id visible for correlation.
func redactClaimNonce(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) != 6 || parts[1] != "api" || !isHexNonce(parts[5]) {
		return p
	}
	switch {
	case parts[2] == "bridges" && parts[4] == "provision",
		parts[2] == "tak" && parts[3] == "enroll":
		parts[5] = "{nonce}"
		return strings.Join(parts, "/")
	}
	return p
}

func isHexNonce(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
