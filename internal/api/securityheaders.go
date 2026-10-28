package api

import (
	"net/http"
	"os"
	"strings"
)

// SecurityHeaders returns middleware that sets security headers on all responses.
// CSP is tuned for the embedded Vue SPA (inline styles from Tailwind, self-hosted scripts).
func SecurityHeaders(next http.Handler) http.Handler {
	forceHSTS := strings.ToLower(os.Getenv("HUB_FORCE_HSTS")) != "false" // default true
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				// 'wasm-unsafe-eval' lets the console compile WebAssembly from a
				// same-origin file (the Codec2 decoder of the voice notes,
				// /vendor/codec2/, MESHSAT-1735) while scripts stay 'self' only;
				// without it Chromium refuses WebAssembly.instantiate under a CSP.
				"script-src 'self' 'wasm-unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline'; "+ // Tailwind and MapLibre set inline styles
				"img-src 'self' data: blob:; "+ // map glyphs and sprites, QR blob URLs, photos of LXMF messages; no third-party tile host since MESHSAT-967
				"connect-src 'self'; "+ // the vector basemap is same-origin at /basemap/
				"font-src 'self'; "+
				"worker-src 'self' blob:; "+ // MapLibre runs its tile workers from a blob URL
				"child-src 'self' blob:; "+ // worker-src fallback for older engines
				"media-src 'self' blob:; "+ // an Opus voice note plays from a blob URL, fetched with the session token (MESHSAT-1735)
				"manifest-src 'self'; "+
				"object-src 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'; "+
				"frame-ancestors 'none'")

		// HSTS — set when TLS is detected, or always when HUB_FORCE_HSTS is not "false".
		proto := r.Header.Get("X-Forwarded-Proto")
		if forceHSTS || r.TLS != nil || proto == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}
