package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func (m *mockStore) UpsertRNSPath(context.Context, *store.RNSPath) error                 { return nil }
func (m *mockStore) ListRNSPaths(context.Context) ([]store.RNSPath, error)               { return nil, nil }
func (m *mockStore) DeleteRNSPath(context.Context, string) error                         { return nil }
func (m *mockStore) InsertLXMFMessage(context.Context, string, *store.LXMFMessage) error { return nil }
func (m *mockStore) GetLXMFMessage(context.Context, string, string) (*store.LXMFMessage, error) {
	return nil, store.ErrNotFound
}
func (m *mockStore) ListLXMFMessages(context.Context, string, store.LXMFQuery) ([]store.LXMFMessage, error) {
	return nil, nil
}
func (m *mockStore) UpdateLXMFMessageState(context.Context, string, string, store.LXMFStateUpdate) error {
	return nil
}
func (m *mockStore) CountLXMFMessages(context.Context, string) (int, error)               { return 0, nil }
func (m *mockStore) PruneLXMFMessages(context.Context, string, int) (int, error)          { return 0, nil }
func (m *mockStore) InsertLXMFHeld(context.Context, string, *store.LXMFHeldMessage) error { return nil }
func (m *mockStore) ListLXMFHeldFor(context.Context, string, string) ([]store.LXMFHeldMessage, error) {
	return nil, nil
}
func (m *mockStore) GetLXMFHeld(context.Context, string, string) (*store.LXMFHeldMessage, error) {
	return nil, store.ErrNotFound
}
func (m *mockStore) DeleteLXMFHeld(context.Context, string, string, []string) (int, error) {
	return 0, nil
}
func (m *mockStore) LXMFHeldBytes(context.Context, string) (int64, error) { return 0, nil }
func (m *mockStore) PruneLXMFHeld(context.Context, string, time.Time, int64) (int, error) {
	return 0, nil
}

func (m *mockStore) AdmitMsgOp(context.Context, *store.MsgOp) (store.MsgOpAdmission, error) {
	return store.MsgOpAdmission{}, nil
}
func (m *mockStore) GetMsgOp(context.Context, string, string, string) (*store.MsgOp, error) {
	return nil, store.ErrNotFound
}
func (m *mockStore) AddMsgOpEvent(context.Context, *store.MsgOpEvent, func([]store.MsgOpEvent) (string, bool)) (bool, error) {
	return false, nil
}
func (m *mockStore) ListMsgOpEvents(context.Context, string, string, string) ([]store.MsgOpEvent, error) {
	return nil, nil
}
func (m *mockStore) ListDueMsgOps(context.Context, time.Time, int) ([]store.MsgOp, error) {
	return nil, nil
}
func (m *mockStore) ClaimMsgOp(context.Context, string, string, string, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (m *mockStore) MarkMsgOpAttempt(context.Context, string, string, string, time.Time) error {
	return nil
}
func (m *mockStore) SetMsgOpRecipientAnswer(context.Context, string, string, string, string) error {
	return nil
}
func (m *mockStore) ListExpiredMsgOps(context.Context, time.Time, int) ([]store.MsgOp, error) {
	return nil, nil
}
func (m *mockStore) ListUnpublishedMsgOpEvents(context.Context, int) ([]store.MsgOpEvent, error) {
	return nil, nil
}
func (m *mockStore) MarkMsgOpEventsPublished(context.Context, []string, time.Time) error { return nil }
func (m *mockStore) ReplayMsgOpEvents(context.Context, string, string, string) (int, error) {
	return 0, nil
}
func (m *mockStore) PruneMsgOps(context.Context, time.Time) (int, error) { return 0, nil }
func (m *mockStore) ApplySOSRunEvent(context.Context, *store.SOSRunEvent) (store.SOSRunApplied, error) {
	return store.SOSRunApplied{}, nil
}
func (m *mockStore) GetSOSRun(context.Context, string, string, string) (*store.SOSRun, error) {
	return nil, store.ErrNotFound
}
func (m *mockStore) FindSOSRuns(context.Context, string, string) ([]store.SOSRun, error) {
	return nil, nil
}
func (m *mockStore) ListSOSRuns(context.Context, string, int) ([]store.SOSRun, error) {
	return nil, nil
}
func (m *mockStore) SetSOSRunAlert(context.Context, string, string, string, string) error { return nil }
func (m *mockStore) ClearSOSRunAlertPending(context.Context, string, string, string, int) (bool, error) {
	return false, nil
}
func (m *mockStore) ListSOSRunsAlertPending(context.Context, int) ([]store.SOSRun, error) {
	return nil, nil
}
