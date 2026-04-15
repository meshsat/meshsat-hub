package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestListDevices(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		store     *mockStore
		wantCode  int
		wantCount int
	}{
		{
			name:      "empty list returns empty array",
			store:     &mockStore{devices: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name: "returns devices",
			store: &mockStore{devices: []store.Device{
				{IMEI: "123456789012345", Label: "Test", Type: "rockblock", CreatedAt: now},
				{IMEI: "987654321098765", Label: "Probe", Type: "globalstar", CreatedAt: now},
			}},
			wantCode:  http.StatusOK,
			wantCount: 2,
		},
		{
			name:     "store error returns 500",
			store:    &mockStore{deviceErr: fmt.Errorf("db down")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices", nil)
			h.ListDevices(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var devices []store.Device
				if err := json.NewDecoder(rec.Body).Decode(&devices); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(devices) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(devices), tt.wantCount)
				}
			}
		})
	}
}

func TestGetDevice(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		store    *mockStore
		imei     string
		wantCode int
	}{
		{
			name:     "found",
			store:    &mockStore{device: &store.Device{IMEI: "123", Label: "Test", CreatedAt: now}},
			imei:     "123",
			wantCode: http.StatusOK,
		},
		{
			name:     "not found",
			store:    &mockStore{device: nil},
			imei:     "999",
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/"+tt.imei, map[string]string{"imei": tt.imei})
			h.GetDevice(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestCreateDevice(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		store    *mockStore
		wantCode int
	}{
		{
			name: "success",
			body: `{"imei":"123456789012345","label":"Test"}`,
			store: &mockStore{
				device: &store.Device{IMEI: "123456789012345", Label: "Test", Type: "rockblock"},
			},
			wantCode: http.StatusCreated,
		},
		{
			name:     "missing imei",
			body:     `{"label":"Test"}`,
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
			name: "defaults type to rockblock",
			body: `{"imei":"123"}`,
			store: &mockStore{
				device: &store.Device{IMEI: "123", Type: "rockblock"},
			},
			wantCode: http.StatusCreated,
		},
		{
			name:     "conflict",
			body:     `{"imei":"123"}`,
			store:    &mockStore{deviceErr: fmt.Errorf("duplicate")},
			wantCode: http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceHandler(tt.store)
			req := httptest.NewRequest(http.MethodPost, "/api/devices", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(withTenant(req.Context(), "test-tenant"))
			rec := httptest.NewRecorder()
			h.CreateDevice(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestUpdateDevice(t *testing.T) {
	existing := &store.Device{IMEI: "123", Label: "Old", Type: "rockblock"}
	tests := []struct {
		name     string
		body     string
		store    *mockStore
		wantCode int
	}{
		{
			name:     "success",
			body:     `{"label":"New","type":"globalstar","notes":"updated"}`,
			store:    &mockStore{device: existing},
			wantCode: http.StatusOK,
		},
		{
			name:     "device not found",
			body:     `{"label":"New"}`,
			store:    &mockStore{device: nil},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "invalid json",
			body:     `{bad`,
			store:    &mockStore{device: existing},
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceHandler(tt.store)
			req := httptest.NewRequest(http.MethodPut, "/api/devices/123", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(withTenant(req.Context(), "test-tenant"))
			req = withChiURLParam(req, "imei", "123")
			rec := httptest.NewRecorder()
			h.UpdateDevice(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestDeleteDevice(t *testing.T) {
	tests := []struct {
		name     string
		store    *mockStore
		wantCode int
	}{
		{
			name:     "success",
			store:    &mockStore{device: &store.Device{IMEI: "123"}},
			wantCode: http.StatusNoContent,
		},
		{
			name:     "not found",
			store:    &mockStore{device: nil},
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceHandler(tt.store)
			req, rec := newTestRequest(http.MethodDelete, "/api/devices/123", map[string]string{"imei": "123"})
			h.DeleteDevice(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}
