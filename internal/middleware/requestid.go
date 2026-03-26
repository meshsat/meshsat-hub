// Package middleware provides HTTP middleware for MeshSat Hub.
package middleware

import (
	"context"
	"net/http"
	"unicode"

	"github.com/rs/xid"
)

type contextKey string

// RequestIDKey is the context key for the request correlation ID.
const RequestIDKey contextKey = "request_id"

// RequestID is a middleware that assigns a unique correlation ID to each request.
// It checks for an incoming X-Request-ID header (max 64 printable ASCII chars)
// and generates a new xid if absent. The ID is stored in the request context
// and echoed in the X-Request-ID response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 || !isPrintableASCII(id) {
			id = xid.New().String()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), RequestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext extracts the correlation ID from a context.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(RequestIDKey).(string); ok {
		return id
	}
	return ""
}

func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
