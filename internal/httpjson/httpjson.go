// Package httpjson holds the three JSON helpers every HTTP handler in the Hub
// uses: WriteJSON, WriteError and the strict ReadJSON decoder.
//
// They lived in internal/api, and any package that wanted them had to import
// the whole handler set -- which made internal/email and internal/routing, both
// of which carry field traffic, transitively depend on internal/quota and every
// other thing internal/api reaches. That in turn forced the SOS-invariant test
// in internal/quota to check direct imports and source text instead of the
// dependency graph. This package has no Hub imports at all, so an ingest package
// can decode a request body without acquiring the subscription ceiling as a
// dependency (MESHSAT-992).
//
// internal/api keeps writeJSON/writeError/readJSON and their exported twins as
// one-line wrappers over these, so no handler changed.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBodySize is the default maximum request body size (1 MB) ReadJSON
// enforces when the caller passes no limit of its own.
const MaxBodySize = 1_048_576

// WriteJSON writes v as a JSON response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"error": msg} with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// ReadJSON decodes a JSON request body into dst with strict validation:
//   - Enforces a maximum body size (default MaxBodySize, or custom via maxBytes)
//   - Rejects unknown fields
//   - Rejects requests with multiple JSON values
//   - Returns a clean, user-safe error message
func ReadJSON(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes ...int64) error {
	limit := int64(MaxBodySize)
	if len(maxBytes) > 0 && maxBytes[0] > 0 {
		limit = maxBytes[0]
	}

	r.Body = http.MaxBytesReader(w, r.Body, limit)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err != nil {
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError
		var maxBytesError *http.MaxBytesError

		switch {
		case errors.As(err, &syntaxError):
			return fmt.Errorf("request body contains malformed JSON (at position %d)", syntaxError.Offset)

		case errors.Is(err, io.ErrUnexpectedEOF):
			return errors.New("request body contains malformed JSON")

		case errors.As(err, &unmarshalTypeError):
			return fmt.Errorf("request body contains invalid value for field %q (at position %d)", unmarshalTypeError.Field, unmarshalTypeError.Offset)

		case strings.HasPrefix(err.Error(), "json: unknown field "):
			fieldName := strings.TrimPrefix(err.Error(), "json: unknown field ")
			return fmt.Errorf("request body contains unknown field %s", fieldName)

		case errors.Is(err, io.EOF):
			return errors.New("request body must not be empty")

		case errors.As(err, &maxBytesError):
			return fmt.Errorf("request body must not be larger than %d bytes", limit)

		default:
			return fmt.Errorf("invalid request body: %s", err.Error())
		}
	}

	// Check for extra data after the first JSON value.
	if dec.More() {
		return errors.New("request body must contain only a single JSON value")
	}

	return nil
}
