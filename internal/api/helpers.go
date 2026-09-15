package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/httpjson"
)

// The JSON helpers live in internal/httpjson so that packages carrying field
// traffic (internal/email, internal/routing) can use them without importing
// this package and, through it, the subscription ceiling. These wrappers exist
// so no handler in this package changed (MESHSAT-992).

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	httpjson.WriteJSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpjson.WriteError(w, status, msg)
}

// WriteJSON is the exported version of writeJSON for use by other packages.
func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	writeJSON(w, status, v)
}

// WriteError is the exported version of writeError for use by other packages.
func WriteError(w http.ResponseWriter, status int, msg string) {
	writeError(w, status, msg)
}

// ReadJSON is the exported version of readJSON for use by other packages.
func ReadJSON(w http.ResponseWriter, r *http.Request, v interface{}, maxBytes ...int64) error {
	return readJSON(w, r, v, maxBytes...)
}
