package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The strict decoder's own table of cases lives with its implementation in
// internal/httpjson. This test pins that the package-local wrapper every
// handler calls still delivers those semantics: the unknown-field rejection
// and the body limit are the two a refactor is most likely to drop.
func TestReadJSONWrapperKeepsStrictSemantics(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"test","unknown":true}`))
	var dst payload
	if err := readJSON(httptest.NewRecorder(), r, &dst); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field was not rejected: %v", err)
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+strings.Repeat("x", 200)+`"}`))
	if err := readJSON(httptest.NewRecorder(), r, &dst, 50); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("body limit was not enforced: %v", err)
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"test"}`))
	if err := readJSON(httptest.NewRecorder(), r, &dst); err != nil || dst.Name != "test" {
		t.Fatalf("valid body refused: %v (%+v)", err, dst)
	}
}
