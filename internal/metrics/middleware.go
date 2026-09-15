package metrics

import (
	"bufio"
	"fmt"
	"net"
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

// Hijack lets a WebSocket upgrade through the wrapper. Embedding the
// http.ResponseWriter INTERFACE promotes only its three methods, so every
// upgrade under this middleware answered 500 "response does not implement
// http.Hijacker": the relay's first live run (MESHSAT-612) and, it turns
// out, the dashboard's own /api/ws since this middleware was added.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.code = http.StatusSwitchingProtocols
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap lets http.ResponseController reach the server's writer, so a
// handler that waits longer than the server's WriteTimeout (an out-of-band
// command over SMS, MESHSAT-1164) can extend its own deadline instead of
// having the connection cut under it with no response.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps streaming responses working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ChiMiddleware records HTTP request duration, count, and active connections
// using chi route patterns.
func ChiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip metrics and healthz paths from recording.
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		HTTPConnectionsActive.Inc()
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

		next.ServeHTTP(sw, r)

		HTTPConnectionsActive.Dec()

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
