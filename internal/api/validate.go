package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBodySize is the default maximum request body size (1 MB).
const maxBodySize = 1_048_576

// readJSON decodes a JSON request body into dst with strict validation:
//   - Enforces a maximum body size (default 1 MB, or custom via maxBytes)
//   - Rejects unknown fields
//   - Rejects requests with multiple JSON values
//   - Returns a clean, user-safe error message
func readJSON(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes ...int64) error {
	limit := int64(maxBodySize)
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
