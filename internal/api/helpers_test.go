package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		value      interface{}
		wantStatus int
	}{
		{
			name:       "ok with map",
			status:     http.StatusOK,
			value:      map[string]string{"key": "value"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "created with struct",
			status:     http.StatusCreated,
			value:      struct{ Name string }{"test"},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "empty array",
			status:     http.StatusOK,
			value:      []string{},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeJSON(rec, tt.status, tt.value)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			ct := rec.Header().Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			// Verify response is valid JSON.
			var parsed interface{}
			if err := json.NewDecoder(rec.Body).Decode(&parsed); err != nil {
				t.Errorf("response is not valid JSON: %v", err)
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		msg    string
	}{
		{
			name:   "bad request",
			status: http.StatusBadRequest,
			msg:    "invalid input",
		},
		{
			name:   "not found",
			status: http.StatusNotFound,
			msg:    "resource not found",
		},
		{
			name:   "internal error",
			status: http.StatusInternalServerError,
			msg:    "something went wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, tt.status, tt.msg)

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			var resp map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp["error"] != tt.msg {
				t.Errorf("error = %q, want %q", resp["error"], tt.msg)
			}
		})
	}
}
