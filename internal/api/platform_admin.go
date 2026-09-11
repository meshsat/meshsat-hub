package api

import (
	"net/http"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
)

// requestIsPlatformAdmin reports whether the caller is a platform
// administrator, for handlers that show platform-only detail inside an
// otherwise tenant-wide response.
func requestIsPlatformAdmin(r *http.Request) bool {
	u := hubauth.FromContext(r.Context())
	return u != nil && u.PlatformAdmin
}
