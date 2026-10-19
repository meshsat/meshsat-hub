package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type fakeCanceller struct {
	tenant, sosID, user string
	runs                []store.SOSRun
	err                 error
}

func (f *fakeCanceller) Cancel(_ context.Context, tenantID, sosID, userID string) ([]store.SOSRun, error) {
	f.tenant, f.sosID, f.user = tenantID, sosID, userID
	return f.runs, f.err
}

// The operator's cancel names the run and answers 404 for one the tenant does not have.
func TestSOSRunCancelRoute(t *testing.T) {
	for _, c := range []struct {
		name string
		f    *fakeCanceller
		code int
	}{
		{"known", &fakeCanceller{runs: []store.SOSRun{{SOSID: "s1", State: store.SOSRunCancelled}}}, http.StatusOK},
		{"unknown", &fakeCanceller{err: store.ErrNotFound}, http.StatusNotFound},
	} {
		h := NewSOSRunHandler(nil, c.f)
		r := chi.NewRouter()
		r.Post("/api/sos/runs/{sos_id}/cancel", h.Cancel)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sos/runs/s1/cancel", nil))
		if rec.Code != c.code || c.f.sosID != "s1" {
			t.Errorf("%s: %d %q", c.name, rec.Code, c.f.sosID)
		}
		if c.code == http.StatusOK {
			var runs []store.SOSRun
			if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil || len(runs) != 1 || runs[0].State != store.SOSRunCancelled {
				t.Errorf("%s: body %s", c.name, rec.Body.String())
			}
		}
	}
}
