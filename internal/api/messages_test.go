package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestListMessages(t *testing.T) {
	now := time.Now()
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
			store:     &mockStore{messages: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:  "returns messages",
			query: "",
			store: &mockStore{messages: []store.Message{
				{ID: "1", DeviceIMEI: "123", Direction: "mo", CreatedAt: now},
				{ID: "2", DeviceIMEI: "123", Direction: "mt", CreatedAt: now},
			}},
			wantCode:  http.StatusOK,
			wantCount: 2,
		},
		{
			name:      "with device filter",
			query:     "?device=123",
			store:     &mockStore{messages: []store.Message{{ID: "1", DeviceIMEI: "123", CreatedAt: now}}},
			wantCode:  http.StatusOK,
			wantCount: 1,
		},
		{
			name:      "with limit",
			query:     "?limit=10",
			store:     &mockStore{messages: []store.Message{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:      "invalid limit ignored",
			query:     "?limit=abc",
			store:     &mockStore{messages: []store.Message{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:     "store error",
			query:    "",
			store:    &mockStore{messageErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewMessageHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/messages"+tt.query, nil)
			h.ListMessages(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var msgs []store.Message
				if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(msgs) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(msgs), tt.wantCount)
				}
			}
		})
	}
}

func TestGetMessage(t *testing.T) {
	tests := []struct {
		name     string
		store    *mockStore
		wantCode int
	}{
		{
			name:     "found",
			store:    &mockStore{message: &store.Message{ID: "msg-1", Direction: "mo"}},
			wantCode: http.StatusOK,
		},
		{
			name:     "not found",
			store:    &mockStore{message: nil},
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewMessageHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/messages/msg-1", map[string]string{"id": "msg-1"})
			h.GetMessage(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}
