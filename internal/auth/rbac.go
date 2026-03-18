package auth

import (
	"fmt"
	"net/http"
)

// Role constants define the RBAC hierarchy: viewer < operator < owner.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleOwner    = "owner"
)

// roleRank maps role names to numeric rank for comparison.
var roleRank = map[string]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleOwner:    3,
	"admin":      3, // legacy alias for owner
}

// RoleAtLeast returns true if the user's highest role meets or exceeds minRole.
func RoleAtLeast(user *User, minRole string) bool {
	if user == nil {
		return false
	}
	minRank, ok := roleRank[minRole]
	if !ok {
		return false
	}
	for _, r := range user.Roles {
		if rank, ok := roleRank[r]; ok && rank >= minRank {
			return true
		}
	}
	return false
}

// RequireRole returns middleware that enforces a minimum role.
// Requests from users without sufficient role get 403 Forbidden.
func RequireRole(minRole string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := FromContext(r.Context())
			if !RoleAtLeast(user, minRole) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"error":"insufficient role, requires %s"}`, minRole)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
