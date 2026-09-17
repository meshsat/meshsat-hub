package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1215 follow-up (posture item 6): minting and revoking an API key are
// audit events. The mock store implements AppendAuditEntry, so the audit
// service writes into m.auditEntries and the test reads the chain back.

func TestCreatingAnAPIKeyIsAudited(t *testing.T) {
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
		k.ID = "key-42"
		return nil
	}}
	h := NewAPIKeyHandler(st)
	h.SetAudit(audit.New(st))
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", strings.NewReader(`{"label":"nightly","role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(callerCtx("t-customer", false))
	rec := httptest.NewRecorder()
	h.CreateKey(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if len(st.auditEntries) != 1 || st.auditEntries[0].Action != "api_key_created" {
		t.Fatalf("expected one api_key_created audit row, got %+v", st.auditEntries)
	}
	e := st.auditEntries[0]
	if !strings.Contains(e.Detail, "id=key-42") || !strings.Contains(e.Detail, "role=viewer") || !strings.Contains(e.Detail, `label="nightly"`) {
		t.Errorf("detail should identify the key without being it: %q", e.Detail)
	}
	if strings.Contains(e.Detail, "meshsat_") && len(e.Detail) > 200 {
		t.Errorf("detail must never carry the plaintext key: %q", e.Detail)
	}
	if e.Actor != "user-1" {
		t.Errorf("actor should be the caller, got %q", e.Actor)
	}
	if e.HashVersion != audit.HashVersionCurrent {
		t.Errorf("audit row written with hash version %d, want %d", e.HashVersion, audit.HashVersionCurrent)
	}
}

func TestDeletingAnAPIKeyIsAudited(t *testing.T) {
	st := &mockStore{}
	h := NewAPIKeyHandler(st)
	h.SetAudit(audit.New(st))
	r := chi.NewRouter()
	r.Delete("/api/auth/keys/{id}", h.DeleteKey)
	req := httptest.NewRequest(http.MethodDelete, "/api/auth/keys/key-42", nil)
	req = req.WithContext(callerCtx("t-customer", false))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if len(st.auditEntries) != 1 || st.auditEntries[0].Action != "api_key_deleted" || !strings.Contains(st.auditEntries[0].Detail, "id=key-42") {
		t.Fatalf("expected one api_key_deleted row naming the key, got %+v", st.auditEntries)
	}
}

// Negative control: a handler with no audit service writes nothing and still
// works -- the audit is additive, never load-bearing for the request.
func TestAPIKeyHandlerWithoutAuditWritesNoRows(t *testing.T) {
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error { k.ID = "k"; return nil }}
	rec := postKey(t, callerCtx("t-customer", false), `{"label":"x","role":"viewer"}`, st)
	if rec.Code != http.StatusCreated || len(st.auditEntries) != 0 {
		t.Fatalf("code=%d rows=%d", rec.Code, len(st.auditEntries))
	}
}
