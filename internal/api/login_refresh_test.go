package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// refreshStore is the mock with a live refresh token and a user, so Refresh
// can be exercised end to end.
type refreshStore struct {
	*mockStore
}

func (s *refreshStore) GetRefreshToken(context.Context, string) (*store.RefreshToken, error) {
	return &store.RefreshToken{ID: "rt1", UserID: "usr_1", TenantID: "default", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (s *refreshStore) GetUserByID(context.Context, string, string) (*store.LocalUser, error) {
	return &store.LocalUser{ID: "usr_1", Email: "admin@meshsat.net", Name: "Kyriakos", Role: "owner", Enabled: true}, nil
}

// A refreshed access token keeps the platform-admin flag of the linked OIDC
// identity. The SPA completes every OIDC login through /api/auth/refresh, so
// without this a platform admin lost the flag on the first page load
// (observed on hub.meshsat.net, 2026-09-08).
func TestRefresh_CarriesPlatformAdmin(t *testing.T) {
	sm := hubauth.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), "test")
	for _, admin := range []bool{true, false} {
		st := &refreshStore{&mockStore{platformAdmin: admin}}
		h := NewLoginHandler(st, sm, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		req.AddCookie(&http.Cookie{Name: "meshsat_refresh", Value: "plain-token"})
		rec := httptest.NewRecorder()
		h.Refresh(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin=%v: status %d body %s", admin, rec.Code, rec.Body.String())
		}
		var resp struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		claims, err := sm.VerifyAccessToken(resp.AccessToken)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if claims.PlatformAdmin != admin {
			t.Errorf("admin=%v: refreshed token PlatformAdmin = %v", admin, claims.PlatformAdmin)
		}
		if claims.TenantID != "default" || claims.Role != "owner" {
			t.Errorf("claims = %+v", claims)
		}
	}
}
