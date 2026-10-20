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

// TestDirectoryGroupAndPolicyREST: the group and policy routes, each write
// moving the version once, and another tenant reaching none of it (the
// victim is the default tenant on purpose). [MESHSAT-1584 C7]
func TestDirectoryGroupAndPolicyREST(t *testing.T) {
	h, s := directoryTestHandler(t)
	ctx := context.Background()
	const victim, other = "default", "t-other"
	if err := s.PutContact(ctx, &directory.Contact{ID: "c1", TenantID: victim, DisplayName: "One"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutContact(ctx, &directory.Contact{ID: "c9", TenantID: other, DisplayName: "Theirs"}); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name, method, tenant, id, body string
		call                           func(http.ResponseWriter, *http.Request)
		want                           int
		version                        int64 // the victim's, after the step
		contains                       string
	}{
		{"create team", http.MethodPost, victim, "", `{"id":"g1","name":"Red","kind":"TEAM","member_ids":["c1"]}`, h.CreateGroup, http.StatusCreated, 2, `"member_ids":["c1"]`},
		{"create with a bad kind", http.MethodPost, victim, "", `{"name":"X","kind":"SQUAD"}`, h.CreateGroup, http.StatusBadRequest, 2, "group kind"},
		{"create naming another tenant's contact", http.MethodPost, victim, "", `{"name":"X","member_ids":["c9"]}`, h.CreateGroup, http.StatusConflict, 2, "not a contact"},
		{"create without a name", http.MethodPost, victim, "", `{"kind":"TEAM"}`, h.CreateGroup, http.StatusBadRequest, 2, "name"},
		{"get", http.MethodGet, victim, "g1", ``, h.GetGroup, http.StatusOK, 2, `"kind":"TEAM"`},
		{"list", http.MethodGet, victim, "", ``, h.ListGroups, http.StatusOK, 2, `"id":"g1"`},
		{"update", http.MethodPut, victim, "g1", `{"name":"Red team","kind":"TEAM","member_ids":[]}`, h.UpdateGroup, http.StatusOK, 3, `"name":"Red team"`},
		{"another tenant reads it", http.MethodGet, other, "g1", ``, h.GetGroup, http.StatusNotFound, 3, ""},
		{"another tenant lists", http.MethodGet, other, "", ``, h.ListGroups, http.StatusOK, 3, "[]"},
		{"another tenant updates it", http.MethodPut, other, "g1", `{"name":"Mine"}`, h.UpdateGroup, http.StatusNotFound, 3, ""},
		{"another tenant deletes it", http.MethodDelete, other, "g1", ``, h.DeleteGroup, http.StatusNotFound, 3, ""},
		{"create policy", http.MethodPost, victim, "", `{"id":"p1","scope_type":"precedence","scope_id":"Flash","strategy":"HEMB_BONDED"}`, h.CreatePolicy, http.StatusCreated, 4, `"strategy":"HEMB_BONDED"`},
		{"second policy for the scope", http.MethodPost, victim, "", `{"scope_type":"precedence","scope_id":"Flash","strategy":"PRIMARY_ONLY"}`, h.CreatePolicy, http.StatusConflict, 4, "scope"},
		{"unknown strategy", http.MethodPost, victim, "", `{"scope_type":"default","strategy":"FASTEST"}`, h.CreatePolicy, http.StatusBadRequest, 4, "strategy"},
		{"get policy", http.MethodGet, victim, "p1", ``, h.GetPolicy, http.StatusOK, 4, `"scope_id":"Flash"`},
		{"update policy", http.MethodPut, victim, "p1", `{"scope_type":"precedence","scope_id":"Flash","strategy":"ALL_BEARERS"}`, h.UpdatePolicy, http.StatusOK, 5, `"strategy":"ALL_BEARERS"`},
		{"another tenant reads the policy", http.MethodGet, other, "p1", ``, h.GetPolicy, http.StatusNotFound, 5, ""},
		{"another tenant lists policies", http.MethodGet, other, "", ``, h.ListPolicies, http.StatusOK, 5, "[]"},
		{"another tenant updates the policy", http.MethodPut, other, "p1", `{"scope_type":"default","strategy":"PRIMARY_ONLY"}`, h.UpdatePolicy, http.StatusNotFound, 5, ""},
		{"another tenant deletes the policy", http.MethodDelete, other, "p1", ``, h.DeletePolicy, http.StatusNotFound, 5, ""},
		{"delete policy", http.MethodDelete, victim, "p1", ``, h.DeletePolicy, http.StatusNoContent, 6, ""},
		{"delete group", http.MethodDelete, victim, "g1", ``, h.DeleteGroup, http.StatusNoContent, 7, ""},
		{"delete group again", http.MethodDelete, victim, "g1", ``, h.DeleteGroup, http.StatusNotFound, 7, ""},
		{"update a missing policy", http.MethodPut, victim, "p1", `{"scope_type":"default","strategy":"PRIMARY_ONLY"}`, h.UpdatePolicy, http.StatusNotFound, 7, ""},
	}
	for _, st := range steps {
		w := httptest.NewRecorder()
		st.call(w, directoryRequest(st.method, "/api/v1/directory/x", st.body, st.tenant, st.id))
		if w.Code != st.want || !strings.Contains(w.Body.String(), st.contains) {
			t.Errorf("%s: %d %s, want %d containing %q", st.name, w.Code, w.Body.String(), st.want, st.contains)
		}
		if v, _ := s.CurrentVersion(ctx, victim); v != st.version {
			t.Errorf("%s: victim version %d, want %d", st.name, v, st.version)
		}
	}
	if g, _ := s.GetGroup(ctx, other, "g1"); g != nil {
		t.Errorf("another tenant ended up with %+v", g)
	}
}
