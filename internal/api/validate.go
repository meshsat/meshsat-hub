package api

import (
	"net/http"
	"strconv"

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

// maxListLimit caps every `?limit=` a list endpoint accepts. A limit is a page
// size, not a licence to stream the table: an unbounded value is one request
// away from a full-table read on the audit log or the message store, which is
// a memory and a database problem before it is anything else.
const maxListLimit = 500

// parseLimit reads `?limit=` and returns it clamped to (0, max]; a missing,
// unparsable or non-positive value returns def, a value above max returns max.
func parseLimit(r *http.Request, def, max int) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
