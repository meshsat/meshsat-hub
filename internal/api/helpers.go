package api

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
