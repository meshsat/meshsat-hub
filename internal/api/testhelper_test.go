package api

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/go-chi/chi/v5"
)

// withTenant returns a context with tenant ID set.
func withTenant(ctx context.Context, tid string) context.Context {
	return context.WithValue(ctx, auth.TenantContextKey, tid)
}

// withUser returns a context with an authenticated user set.
func withUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, auth.UserContextKey, u)
}

// withChiURLParam adds a chi URL parameter to the request.
func withChiURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// newTestRequest creates an httptest request with tenant context and optional chi URL params.
func newTestRequest(method, target string, params map[string]string) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, target, nil)
	req = req.WithContext(withTenant(req.Context(), "test-tenant"))

	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}

	return req, httptest.NewRecorder()
}
