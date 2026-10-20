package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/directory"

	_ "modernc.org/sqlite"
)

func directoryTestHandler(t *testing.T) (*DirectoryHandler, *directory.SQLStore) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := directory.NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewDirectoryHandler(s, nil), s
}

func directoryRequest(method, target, body, tenant, id string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(r.Context(), auth.TenantContextKey, tenant)
	if id != "" {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
	}
	return r.WithContext(ctx)
}

// TestDirectoryWritesAnswerTheCallersMistakesAs4xx: a caller's mistake is
// a 400 or 409 with the store's reason, not a 500, and each write moves the
// tenant's version exactly once (the store bumps inside the write; the
// handler no longer bumps a second time). [MESHSAT-1584 C7]
func TestDirectoryWritesAnswerTheCallersMistakesAs4xx(t *testing.T) {
	h, s := directoryTestHandler(t)
	ctx := context.Background()
	steps := []struct {
		name, method, body, id string
		call                   func(http.ResponseWriter, *http.Request)
		want                   int
		version                int64
	}{
		{"create", http.MethodPost, `{"id":"c1","display_name":"One","addresses":[{"kind":"SMS","value":"+31611111111"}]}`, "",
			h.CreateContact, http.StatusCreated, 1},
		{"create with a taken address", http.MethodPost, `{"display_name":"Two","addresses":[{"kind":"SMS","value":"+31611111111"}]}`, "",
			h.CreateContact, http.StatusConflict, 1},
		{"create with a bad kind", http.MethodPost, `{"display_name":"Two","addresses":[{"kind":"BOGUS","value":"x"}]}`, "",
			h.CreateContact, http.StatusBadRequest, 1},
		{"update", http.MethodPut, `{"display_name":"One again","addresses":[{"kind":"SMS","value":"+31611111111"}]}`, "c1",
			h.UpdateContact, http.StatusOK, 2},
		{"update a missing contact", http.MethodPut, `{"display_name":"x"}`, "nope",
			h.UpdateContact, http.StatusNotFound, 2},
		{"delete", http.MethodDelete, ``, "c1", h.DeleteContact, http.StatusNoContent, 3},
		{"delete again", http.MethodDelete, ``, "c1", h.DeleteContact, http.StatusNotFound, 3},
	}
	for _, st := range steps {
		w := httptest.NewRecorder()
		st.call(w, directoryRequest(st.method, "/api/v1/directory/contacts", st.body, "acme", st.id))
		if w.Code != st.want {
			t.Errorf("%s: %d %s, want %d", st.name, w.Code, w.Body.String(), st.want)
		}
		if v, _ := s.CurrentVersion(ctx, "acme"); v != st.version {
			t.Errorf("%s: version %d, want %d", st.name, v, st.version)
		}
	}
}
