package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The customer's side of hosted TAK: turning it on and reading its phase. The
// accounts, enrolment and their ceiling went with public port 8089 (MESHSAT-1466).

type fakeTAKProv struct {
	calls  int
	label  string
	err    error
	lastID string
}

func (f *fakeTAKProv) EnableTAK(_ context.Context, tenantID string) (string, error) {
	f.calls++
	f.lastID = tenantID
	if f.err != nil {
		return "", f.err
	}
	if f.label == "" {
		f.label = "abcdefghij"
	}
	return f.label, nil
}

// takHandlerOn builds the handler over a mock store.
func takHandlerOn(t *testing.T, m *mockStore) *TenantTAKHandler {
	t.Helper()
	// A nil audit service is handled: logAudit returns early, and a customer's
	// request must never fail because an audit write did.
	return NewTenantTAKHandler(m, nil)
}

func takRequest(method, target string, body string) (*http.Request, *httptest.ResponseRecorder) {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(withTenant(req.Context(), "test-tenant"))
	return req, httptest.NewRecorder()
}

func TestTAKStatusSaysDisabledWhenTheTenantHasNoServer(t *testing.T) {
	m := &mockStore{} // takInstance nil -> GetTAKInstance returns ErrNotFound
	h := takHandlerOn(t, m)

	req, rec := takRequest(http.MethodGet, "/api/tenant/tak", "")
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out takStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Enabled {
		t.Error("enabled = true with no instance")
	}
}

// The in-cluster address must never reach a customer. It is a service name only
// the Hub dials, and publishing it says something about the cluster's innards.
// Nor is there a port any more: no phone connects to the Hub's TAK port since
// MESHSAT-1466, so a port in this answer would invite a customer to try.
func TestTAKStatusNeverLeaksTheInClusterAddress(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{
		TenantID: "test-tenant", Label: "abcdefghij", State: "Running",
		Phase: "Ready", Host: "tak-abcdefghij.meshsat-tak.svc:8089",
		CACertPEM: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n",
	}}
	h := takHandlerOn(t, m)

	req, rec := takRequest(http.MethodGet, "/api/tenant/tak", "")
	h.Status(rec, req)

	body := rec.Body.String()
	for _, leak := range []string{".svc", "meshsat-tak", "abcdefghij", "BEGIN CERTIFICATE"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response carries %q, which a customer must not be shown:\n%s", leak, body)
		}
	}
	var out takStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Enabled || out.Phase != "Ready" {
		t.Errorf("enabled=%v phase=%q, want true/Ready", out.Enabled, out.Phase)
	}
	for _, gone := range []string{"8089", `"port"`, `"users"`, `"limit"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the response still carries %s, a phone connection detail retired with port 8089:\n%s", gone, body)
		}
	}
}

func TestEnablingTAKWithoutTheMachinerySaysSo(t *testing.T) {
	h := takHandlerOn(t, &mockStore{}) // no SetTAK
	req, rec := takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 on a Hub with no hosted TAK", rec.Code)
	}
}

func TestEnablingTAKIsAcceptedAndThenIdempotent(t *testing.T) {
	m := &mockStore{}
	h := takHandlerOn(t, m)
	prov := &fakeTAKProv{}
	h.SetTAK(prov)

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first enable = %d, want 202", rec.Code)
	}
	if prov.calls != 1 || prov.lastID != "test-tenant" {
		t.Errorf("provisioner calls = %d for %q, want 1 for test-tenant", prov.calls, prov.lastID)
	}

	// Second time, with the row now present: still fine, and 200 rather than 202,
	// because nothing new is being built. A double-click must not read as an error.
	m.takInstance = &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij", Phase: "Ready"}
	req, rec = takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("second enable = %d, want 200", rec.Code)
	}
}

// The role gate, exercised through the real RequireRole and the routes as main.go
// mounts them -- so this tests the wiring, not only the handler.
func TestOnlyAnOwnerMayChangeTAK(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{})

	r := chi.NewRouter()
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/tak", h.Status)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/tak", h.Enable)
	})

	cases := []struct {
		name       string
		role       string
		method     string
		target     string
		body       string
		wantDenied bool
	}{
		{"a viewer may look", hubauth.RoleViewer, http.MethodGet, "/api/tenant/tak", "", false},
		{"an operator may not turn it on", hubauth.RoleOperator, http.MethodPost, "/api/tenant/tak", "", true},
		{"an owner may turn it on", hubauth.RoleOwner, http.MethodPost, "/api/tenant/tak", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.target, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			}
			ctx := withTenant(req.Context(), "test-tenant")
			ctx = withUser(ctx, &hubauth.User{
				ID: "u1", Email: "someone@example.test",
				Roles: []string{tc.role}, TenantID: "test-tenant",
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req.WithContext(ctx))

			denied := rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized
			if denied != tc.wantDenied {
				t.Errorf("status = %d (denied=%v), want denied=%v", rec.Code, denied, tc.wantDenied)
			}
		})
	}
}
