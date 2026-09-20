package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
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
			name: "conflict",
			body: `{"imei":"123"}`,
			// A conflict means the IMEI is on file. Before the handler could
			// ask, this fixture modelled one with a bare error and no owner,
			// which is now correctly a 500: an insert that fails while NO such
			// device exists is a store failure, not a duplicate.
			store:    &mockStore{deviceErr: fmt.Errorf("duplicate"), deviceOwner: "test-tenant"},
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

// A second registration of an IMEI is told what actually happened.
//
// imei is the primary key across the WHOLE platform (postgres/schema.go says
// why: a satellite provider delivers by IMEI with no tenant context). The cost
// of that invariant lands on the second tenant to register a kit, who used to
// get the raw database error text as a 409 -- the same message whether the
// device was already theirs, belonged to somebody else, or the database had
// simply failed.
func TestCreateDeviceSaysWhoseTheIMEIIs(t *testing.T) {
	dup := errors.New(`pq: duplicate key value violates unique constraint "devices_pkey"`)
	for _, tc := range []struct {
		name     string
		store    *mockStore
		wantCode int
		wantBody string
		notBody  string
	}{
		{
			name:     "already registered in MY account",
			store:    &mockStore{deviceErr: dup, deviceOwner: "test-tenant"},
			wantCode: http.StatusConflict,
			wantBody: "already registered in your account",
		},
		{
			name:     "registered to ANOTHER account, which is not named",
			store:    &mockStore{deviceErr: dup, deviceOwner: "t-somebody-else"},
			wantCode: http.StatusConflict,
			wantBody: "another account",
			notBody:  "t-somebody-else",
		},
		{
			name:     "ambiguous owner reads as another account too",
			store:    &mockStore{deviceErr: dup, deviceOwnerErr: store.ErrAmbiguousTenant},
			wantCode: http.StatusConflict,
			wantBody: "another account",
		},
		{
			// A database failure is not a conflict, and its text is not for the
			// customer. It used to be a 409 carrying the driver's error string.
			name:     "a store failure is a 500 with no driver text",
			store:    &mockStore{deviceErr: errors.New("pq: connection refused")},
			wantCode: http.StatusInternalServerError,
			wantBody: "could not register",
			notBody:  "pq:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewDeviceHandler(tc.store)
			req, rec := newTestRequest(http.MethodPost, "/api/devices", nil)
			req.Body = http.NoBody
			req = req.WithContext(req.Context())
			req.Body = io.NopCloser(strings.NewReader(`{"imei":"300000000000003"}`))
			req.Header.Set("Content-Type", "application/json")
			h.CreateDevice(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not say %q", rec.Body.String(), tc.wantBody)
			}
			if tc.notBody != "" && strings.Contains(rec.Body.String(), tc.notBody) {
				t.Errorf("body %q leaks %q", rec.Body.String(), tc.notBody)
			}
		})
	}
}
