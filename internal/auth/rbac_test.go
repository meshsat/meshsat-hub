package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setUserContext(ctx context.Context, user *User) context.Context {
	return context.WithValue(ctx, UserContextKey, user)
}

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		name    string
		roles   []string
		minRole string
		want    bool
	}{
		{"owner meets owner", []string{"owner"}, RoleOwner, true},
		{"owner meets operator", []string{"owner"}, RoleOperator, true},
		{"owner meets viewer", []string{"owner"}, RoleViewer, true},
		{"operator meets operator", []string{"operator"}, RoleOperator, true},
		{"operator meets viewer", []string{"operator"}, RoleViewer, true},
		{"operator denied owner", []string{"operator"}, RoleOwner, false},
		{"viewer meets viewer", []string{"viewer"}, RoleViewer, true},
		{"viewer denied operator", []string{"viewer"}, RoleOperator, false},
		{"viewer denied owner", []string{"viewer"}, RoleOwner, false},
		{"admin alias meets owner", []string{"admin"}, RoleOwner, true},
		{"no roles denied", []string{}, RoleViewer, false},
		{"nil user denied", nil, RoleViewer, false},
		{"unknown role ignored", []string{"unknown"}, RoleViewer, false},
		{"multi-role highest wins", []string{"viewer", "operator"}, RoleOperator, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var user *User
			if tt.roles != nil {
				user = &User{ID: "u1", Roles: tt.roles}
			}
			if got := RoleAtLeast(user, tt.minRole); got != tt.want {
				t.Errorf("RoleAtLeast(%v, %q) = %v, want %v", tt.roles, tt.minRole, got, tt.want)
			}
		})
	}
}

func TestRequireRole_Allowed(t *testing.T) {
	inner := okHandler()
	mw := RequireRole(RoleOperator)

	req := httptest.NewRequest("GET", "/api/devices", nil)
	user := &User{ID: "u1", Roles: []string{"owner"}}
	req = req.WithContext(setUserContext(req.Context(), user))

	w := httptest.NewRecorder()
	mw(inner).ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireRole_Denied(t *testing.T) {
	inner := okHandler()
	mw := RequireRole(RoleOwner)

	req := httptest.NewRequest("GET", "/api/auth/keys", nil)
	user := &User{ID: "u1", Roles: []string{"viewer"}}
	req = req.WithContext(setUserContext(req.Context(), user))

	w := httptest.NewRecorder()
	mw(inner).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireRole_NoUser(t *testing.T) {
	inner := okHandler()
	mw := RequireRole(RoleViewer)

	req := httptest.NewRequest("GET", "/api/devices", nil)
	w := httptest.NewRecorder()
	mw(inner).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for no user, got %d", w.Code)
	}
}
