package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestCreateKey(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		store    *mockStore
		wantCode int
	}{
		{
			name: "success with defaults",
			body: `{"label":"test key"}`,
			store: &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
				k.ID = "key-1"
				return nil
			}},
			wantCode: http.StatusCreated,
		},
		{
			name: "explicit role",
			body: `{"label":"ops key","role":"operator"}`,
			store: &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
				k.ID = "key-2"
				return nil
			}},
			wantCode: http.StatusCreated,
		},
		{
			name:     "invalid role",
			body:     `{"label":"bad","role":"superadmin"}`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "invalid json",
			body:     `{bad`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "with expiry",
			body: `{"label":"expiring","expires_in":"720h"}`,
			store: &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
				k.ID = "key-3"
				return nil
			}},
			wantCode: http.StatusCreated,
		},
		{
			name:     "invalid expiry",
			body:     `{"label":"bad","expires_in":"not-a-duration"}`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "store error",
			body:     `{"label":"fail"}`,
			store:    &mockStore{apiKeyErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewAPIKeyHandler(tt.store)
			req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(withTenant(req.Context(), "test-tenant"))
			rec := httptest.NewRecorder()
			h.CreateKey(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusCreated {
				var resp createKeyResponse
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp.Key == "" {
					t.Error("expected plaintext key in response")
				}
				if resp.KeyPrefix == "" {
					t.Error("expected key prefix in response")
				}
			}
		})
	}
}

func TestListKeys(t *testing.T) {
	tests := []struct {
		name      string
		store     *mockStore
		wantCode  int
		wantCount int
	}{
		{
			name:      "empty list",
			store:     &mockStore{apiKeys: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name: "returns keys",
			store: &mockStore{apiKeys: []store.APIKey{
				{ID: "1", KeyPrefix: "meshsat_abc", Role: "viewer", Label: "test"},
				{ID: "2", KeyPrefix: "meshsat_def", Role: "owner", Label: "admin"},
			}},
			wantCode:  http.StatusOK,
			wantCount: 2,
		},
		{
			name:     "store error",
			store:    &mockStore{apiKeyErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewAPIKeyHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/auth/keys", nil)
			h.ListKeys(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var keys []store.APIKey
				if err := json.NewDecoder(rec.Body).Decode(&keys); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(keys) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(keys), tt.wantCount)
				}
			}
		})
	}
}

func TestDeleteKey(t *testing.T) {
	tests := []struct {
		name     string
		keyID    string
		store    *mockStore
		wantCode int
	}{
		{
			name:     "success",
			keyID:    "key-1",
			store:    &mockStore{},
			wantCode: http.StatusNoContent,
		},
		{
			name:     "missing id",
			keyID:    "",
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "store error",
			keyID:    "key-1",
			store:    &mockStore{apiKeyErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewAPIKeyHandler(tt.store)
			req, rec := newTestRequest(http.MethodDelete, "/api/auth/keys/"+tt.keyID, map[string]string{"id": tt.keyID})
			h.DeleteKey(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}
