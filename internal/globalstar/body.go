package globalstar

import (
	"bytes"
	"io"
	"net/http"
)

// readBody reads the full request body and returns the bytes.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// readCloser wraps a byte slice in an io.ReadCloser.
func readCloser(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}
