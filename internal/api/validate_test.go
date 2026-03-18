package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSON(t *testing.T) {
	type testPayload struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "valid JSON",
			body: `{"name":"test","value":42}`,
		},
		{
			name:    "empty body",
			body:    "",
			wantErr: "request body must not be empty",
		},
		{
			name:    "malformed JSON",
			body:    `{"name":}`,
			wantErr: "malformed JSON",
		},
		{
			name:    "unknown field",
			body:    `{"name":"test","unknown":true}`,
			wantErr: "unknown field",
		},
		{
			name:    "wrong type",
			body:    `{"name":"test","value":"notanint"}`,
			wantErr: "invalid value for field",
		},
		{
			name:    "multiple JSON values",
			body:    `{"name":"a"}{"name":"b"}`,
			wantErr: "single JSON value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			w := httptest.NewRecorder()

			var dst testPayload
			err := readJSON(w, r, &dst)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				if dst.Name != "test" || dst.Value != 42 {
					t.Fatalf("unexpected values: %+v", dst)
				}
			} else {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q should contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestReadJSON_MaxBodySize(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+strings.Repeat("x", 200)+`"}`))
	w := httptest.NewRecorder()

	var dst struct {
		Name string `json:"name"`
	}
	err := readJSON(w, r, &dst, 50) // 50 byte limit
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error %q should mention size limit", err.Error())
	}
}
