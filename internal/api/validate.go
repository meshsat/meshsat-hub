package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/httpjson"
)

// readJSON decodes a JSON request body into dst with strict validation:
//   - Enforces a maximum body size (default 1 MB, or custom via maxBytes)
//   - Rejects unknown fields
//   - Rejects requests with multiple JSON values
//   - Returns a clean, user-safe error message
//
// The implementation is httpjson.ReadJSON; this wrapper keeps every handler
// in the package unchanged.
func readJSON(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes ...int64) error {
	return httpjson.ReadJSON(w, r, dst, maxBytes...)
}
