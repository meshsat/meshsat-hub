package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
)

func TestAuthMeHandler(t *testing.T) {
	tests := []struct {
		name     string
		user     *auth.User
		wantCode int
	}{
		{
			name: "authenticated user",
			user: &auth.User{
				ID:       "user-1",
				Email:    "test@example.com",
				Name:     "Test User",
				Roles:    []string{"owner"},
				TenantID: "tenant-1",
			},
			wantCode: http.StatusOK,
		},
		{
			name:     "no user in context",
			user:     nil,
			wantCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			ctx := withTenant(req.Context(), "tenant-1")
			if tt.user != nil {
				ctx = withUser(ctx, tt.user)
			}
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()

			AuthMeHandler(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var resp map[string]interface{}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp["id"] != tt.user.ID {
					t.Errorf("id = %v, want %v", resp["id"], tt.user.ID)
				}
				if resp["email"] != tt.user.Email {
					t.Errorf("email = %v, want %v", resp["email"], tt.user.Email)
				}
				if resp["name"] != tt.user.Name {
					t.Errorf("name = %v, want %v", resp["name"], tt.user.Name)
				}
				if resp["tenant_id"] != "tenant-1" {
					t.Errorf("tenant_id = %v, want tenant-1", resp["tenant_id"])
				}
			}
		})
	}
}
