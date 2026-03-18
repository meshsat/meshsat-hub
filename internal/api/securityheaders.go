package api

import "net/http"

// SecurityHeaders returns middleware that sets security headers on all responses.
// CSP is tuned for the embedded Vue SPA (inline styles from Tailwind, self-hosted scripts).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+ // Tailwind injects inline styles
				"img-src 'self' data: https://*.tile.openstreetmap.org; "+ // Leaflet map tiles
				"connect-src 'self'; "+
				"font-src 'self'; "+
				"object-src 'none'; "+
				"frame-ancestors 'none'")

		// HSTS — only set when request arrived over TLS (behind reverse proxy
		// the X-Forwarded-Proto header indicates the original scheme).
		proto := r.Header.Get("X-Forwarded-Proto")
		if r.TLS != nil || proto == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}
