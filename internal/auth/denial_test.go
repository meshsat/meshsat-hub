package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// counterValue reads a single series out of the real /metrics scrape.
//
// Deliberately not prometheus/client_golang's testutil: it would add
// kylelemons/godebug to the module graph for a two-line job, and this codebase
// justifies every dependency. Scraping is also the more faithful assertion --
// an alert evaluates the exposed text, not the in-process counter, so a series
// that exists in memory and never reaches /metrics would pass a testutil check
// and fail in production.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		ok := true
		for k, v := range labels {
			if !strings.Contains(line, k+`="`+v+`"`) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		i := strings.LastIndex(line, " ")
		f, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		return f
	}
	return -1 // absent: distinguishable from zero, which is the whole point
}

// seriesCount counts the exposed series of a metric family.
func seriesCount(t *testing.T, name string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	n := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, name+"{") {
			n++
		}
	}
	return n
}

// A refused request must leave a trace. For months it left none: no counter,
// no log line, no audit row, because the auth middleware is registered outside
// both the metrics middleware and the request logger and short-circuits before
// either runs (MESHSAT-1190).
//
// These tests assert the SIGNAL, deliberately, not the middleware order. The
// order is a legitimate design choice -- the logger is innermost so it can see
// the authenticated user -- and pinning it would forbid the fix rather than
// protect it. What must not regress is that a refusal reports itself.

func TestAuthFailureIsCounted(t *testing.T) {
	handler := Middleware(Config{Mode: "token", Token: "secret123"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("request reached the handler; it should have been refused")
		}))

	for _, tc := range []struct {
		name, header, reason string
	}{
		{"no credential at all", "", denyMissingCredential},
		{"a wrong bearer", "Bearer wrong-token", denyInvalidToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lbl := map[string]string{"reason": tc.reason, "channel": "internet"}
			before := counterValue(t, "meshsat_hub_auth_failures_total", lbl)

			req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			after := counterValue(t, "meshsat_hub_auth_failures_total", lbl)
			if after != before+1 {
				t.Errorf("meshsat_hub_auth_failures_total{reason=%q} went %v -> %v, want +1. "+
					"A rejected credential that increments nothing is a silent event, which is the whole bug.",
					tc.reason, before, after)
			}
		})
	}
}

func TestAuthzDenialIsCounted(t *testing.T) {
	// A viewer reaching for an owner-only route: authenticated, not authorised.
	handler := RequireRole(RoleOwner)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("request reached the handler; it should have been denied")
		}))

	lbl := map[string]string{"requirement": RoleOwner, "channel": "internet"}
	before := counterValue(t, "meshsat_hub_authz_denials_total", lbl)

	req := httptest.NewRequest(http.MethodPost, "/api/bridges/b1/provision", nil)
	req = req.WithContext(context.WithValue(req.Context(), UserContextKey,
		&User{ID: "u1", Roles: []string{RoleViewer}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	after := counterValue(t, "meshsat_hub_authz_denials_total", lbl)
	if after != before+1 {
		t.Errorf("meshsat_hub_authz_denials_total{requirement=%q} went %v -> %v, want +1",
			RoleOwner, before, after)
	}
}

// The counters must exist before anything increments them. An alert written as
// increase(...[5m]) > N has nothing to evaluate against an absent series, so a
// CounterVec that only materialises on first use is an alert that cannot fire
// until the event it watches for has already happened once.
func TestSecurityCountersExistBeforeFirstUse(t *testing.T) {
	// Asserting on the materialised COUNT rather than by touching a label
	// combination, because WithLabelValues would create the series it is
	// meant to be checking for.
	if got := seriesCount(t, "meshsat_hub_auth_failures_total"); got < 12 {
		t.Errorf("meshsat_hub_auth_failures_total has %d series, want at least 12 "+
			"(6 reasons x 2 channels materialised at startup)", got)
	}
	if got := seriesCount(t, "meshsat_hub_authz_denials_total"); got < 14 {
		t.Errorf("meshsat_hub_authz_denials_total has %d series, want at least 14 "+
			"(7 requirements x 2 channels materialised at startup)", got)
	}
}

// The client address a refusal reports must come from the trusted-proxy walk,
// not from RemoteAddr, which behind the VPS edge and ingress-nginx is always
// the ingress pod.
func TestClientIPTravelsThroughContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	if got := ClientIPFromContext(req.Context()); got != "" {
		t.Errorf("ClientIPFromContext on a bare request = %q, want empty", got)
	}
	req = req.WithContext(WithClientIP(req.Context(), "203.0.113.7"))
	if got := ClientIPFromContext(req.Context()); got != "203.0.113.7" {
		t.Errorf("ClientIPFromContext = %q, want 203.0.113.7", got)
	}
	if got := clientIPOrUnknown(req); got != "203.0.113.7" {
		t.Errorf("clientIPOrUnknown = %q, want 203.0.113.7", got)
	}
	// With nothing stored it must say so rather than name our own ingress pod.
	bare := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	if got := clientIPOrUnknown(bare); got != "unknown" {
		t.Errorf("clientIPOrUnknown with no value = %q, want %q", got, "unknown")
	}
}
