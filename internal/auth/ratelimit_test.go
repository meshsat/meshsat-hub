package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MESHSAT-1219 (ASVS V4): authenticated requests are budgeted per principal;
// requests without a principal are not touched by that budget.

func withUser(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), UserContextKey, &User{ID: id, TenantID: "t"}))
}

func TestPrincipalBudgetRefusesTheRequestAfterTheLimit(t *testing.T) {
	h := PrincipalRateLimit(3)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	codes := []int{}
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withUser(httptest.NewRequest("GET", "/api/devices", nil), "user-a"))
		codes = append(codes, rec.Code)
	}
	want := []int{200, 200, 200, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("call %d: got %d, want %d (all: %v)", i+1, codes[i], want[i], codes)
		}
	}
	// Another principal has its own budget.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withUser(httptest.NewRequest("GET", "/api/devices", nil), "user-b"))
	if rec.Code != 200 {
		t.Fatalf("a different principal must not share the budget, got %d", rec.Code)
	}
}

// The webhooks, the health probes and the capability URLs carry no principal.
// Nothing here may ever answer them 429 -- an SOS arrives on a webhook.
func TestPrincipalBudgetIgnoresRequestsWithoutAPrincipal(t *testing.T) {
	h := PrincipalRateLimit(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/webhook/cloudloop/x", nil))
		if rec.Code != 200 {
			t.Fatalf("call %d without a principal got %d", i+1, rec.Code)
		}
	}
}

func TestPrincipalBudgetZeroDisables(t *testing.T) {
	h := PrincipalRateLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withUser(httptest.NewRequest("GET", "/api/devices", nil), "user-a"))
		if rec.Code != 200 {
			t.Fatalf("budget 0 must disable, got %d", rec.Code)
		}
	}
}

// Invalid API keys from one address stop reaching the validator (and so the
// database) once the failure budget is spent; the answer stays a plain 401.
func TestInvalidAPIKeysAreCutOffBeforeTheValidator(t *testing.T) {
	calls := 0
	validate := func(_ context.Context, _ string) (*User, string, error) {
		calls++
		return nil, "", errors.New("no such key")
	}
	h := APIKeyMiddleware(validate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	ip := "203.0.113.77"
	for i := 0; i < apiKeyFailuresPerMin+10; i++ {
		req := httptest.NewRequest("GET", "/api/devices", nil)
		req.Header.Set("Authorization", "Bearer meshsat_0000000000000000000000000000000000000000000000000000000000000000")
		req = req.WithContext(context.WithValue(req.Context(), clientIPCtxKey{}, ip))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("call %d: got %d, want 401 before and after the cut-off", i+1, rec.Code)
		}
	}
	if calls != apiKeyFailuresPerMin {
		t.Fatalf("validator called %d times, want exactly %d (the budget); the rest must be refused without a lookup", calls, apiKeyFailuresPerMin)
	}
}
