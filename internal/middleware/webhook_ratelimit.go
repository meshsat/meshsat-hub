package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// ipCounter tracks request count and window start for a single IP.
type ipCounter struct {
	count     int
	windowEnd time.Time
}

// WebhookRateLimit returns middleware that applies per-IP rate limiting.
// rpm is the maximum number of requests per minute per source IP.
// Requests exceeding the limit receive HTTP 429 Too Many Requests.
func WebhookRateLimit(next http.Handler, rpm int) http.Handler {
	var mu sync.Mutex
	counters := make(map[string]*ipCounter)

	// Background cleanup goroutine — removes expired entries every 2 minutes.
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			mu.Lock()
			for ip, c := range counters {
				if now.After(c.windowEnd) {
					delete(counters, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}

		mu.Lock()
		now := time.Now()
		c, ok := counters[ip]
		if !ok || now.After(c.windowEnd) {
			c = &ipCounter{
				count:     0,
				windowEnd: now.Add(time.Minute),
			}
			counters[ip] = c
		}
		c.count++
		exceeded := c.count > rpm
		mu.Unlock()

		if exceeded {
			slog.Warn("webhook: rate limit exceeded", "ip", ip, "rpm", rpm)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
