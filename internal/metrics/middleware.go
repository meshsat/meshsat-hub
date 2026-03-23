package metrics

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// ChiMiddleware records HTTP request duration and count using chi route patterns.
func ChiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip metrics and healthz paths from recording.
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

		next.ServeHTTP(sw, r)

		// Use the chi route pattern to avoid high-cardinality labels.
		pattern := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			if rp := rctx.RoutePattern(); rp != "" {
				pattern = rp
			}
		}

		status := fmt.Sprintf("%d", sw.code)
		elapsed := time.Since(start).Seconds()

		HTTPRequestDuration.WithLabelValues(r.Method, pattern, status).Observe(elapsed)
		HTTPRequestsTotal.WithLabelValues(r.Method, pattern, status).Inc()
	})
}
