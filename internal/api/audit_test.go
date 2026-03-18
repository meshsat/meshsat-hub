package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestListEntries(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		store     *mockStore
		wantCode  int
		wantCount int
	}{
		{
			name:      "empty list",
			query:     "",
			store:     &mockStore{auditEntries: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:  "returns entries",
			query: "",
			store: &mockStore{auditEntries: []store.AuditEntry{
				{ID: "1", Action: "device.create", Actor: "user1"},
				{ID: "2", Action: "key.create", Actor: "user1"},
			}},
			wantCode:  http.StatusOK,
			wantCount: 2,
		},
		{
			name:      "with limit",
			query:     "?limit=50",
			store:     &mockStore{auditEntries: []store.AuditEntry{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:     "store error",
			query:    "",
			store:    &mockStore{auditErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := audit.New(tt.store)
			h := NewAuditHandler(svc)
			req, rec := newTestRequest(http.MethodGet, "/api/audit"+tt.query, nil)
			h.ListEntries(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var entries []store.AuditEntry
				if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(entries) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(entries), tt.wantCount)
				}
			}
		})
	}
}

func TestVerifyChain(t *testing.T) {
	tests := []struct {
		name      string
		store     *mockStore
		wantCode  int
		wantValid bool
	}{
		{
			name:      "empty chain is valid",
			store:     &mockStore{auditEntries: []store.AuditEntry{}},
			wantCode:  http.StatusOK,
			wantValid: true,
		},
		{
			name:     "store error",
			store:    &mockStore{auditErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := audit.New(tt.store)
			h := NewAuditHandler(svc)
			req, rec := newTestRequest(http.MethodGet, "/api/audit/verify", nil)
			h.VerifyChain(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var result map[string]interface{}
				if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
					t.Fatalf("decode: %v", err)
				}
				valid, ok := result["valid"].(bool)
				if !ok || valid != tt.wantValid {
					t.Errorf("valid = %v, want %v", result["valid"], tt.wantValid)
				}
			}
		})
	}
}
