package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestGetLatestConfig(t *testing.T) {
	tests := []struct {
		name     string
		store    *mockStore
		wantCode int
	}{
		{
			name: "found",
			store: &mockStore{deviceConfig: &store.DeviceConfig{
				ID: "1", DeviceIMEI: "123", Version: 3, Config: `{"interval":60}`,
			}},
			wantCode: http.StatusOK,
		},
		{
			name:     "no config",
			store:    &mockStore{configErr: sql.ErrNoRows},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "store error",
			store:    &mockStore{configErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceConfigHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/123/config", map[string]string{"imei": "123"})
			h.GetLatest(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestGetConfigVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		store    *mockStore
		wantCode int
	}{
		{
			name:    "found",
			version: "2",
			store: &mockStore{deviceConfig: &store.DeviceConfig{
				ID: "1", DeviceIMEI: "123", Version: 2, Config: `{"interval":30}`,
			}},
			wantCode: http.StatusOK,
		},
		{
			name:     "invalid version",
			version:  "abc",
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "zero version",
			version:  "0",
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "negative version",
			version:  "-1",
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "not found",
			version:  "99",
			store:    &mockStore{configErr: sql.ErrNoRows},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "store error",
			version:  "1",
			store:    &mockStore{configErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceConfigHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/123/config/"+tt.version,
				map[string]string{"imei": "123", "version": tt.version})
			h.GetVersion(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestListConfigVersions(t *testing.T) {
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
			store:     &mockStore{deviceConfigs: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:  "returns versions",
			query: "",
			store: &mockStore{deviceConfigs: []store.DeviceConfig{
				{ID: "1", Version: 1, Config: `{}`},
				{ID: "2", Version: 2, Config: `{}`},
			}},
			wantCode:  http.StatusOK,
			wantCount: 2,
		},
		{
			name:      "with limit",
			query:     "?limit=10",
			store:     &mockStore{deviceConfigs: []store.DeviceConfig{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:     "store error",
			query:    "",
			store:    &mockStore{configErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceConfigHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/123/config/history"+tt.query,
				map[string]string{"imei": "123"})
			h.ListVersions(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var configs []store.DeviceConfig
				if err := json.NewDecoder(rec.Body).Decode(&configs); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(configs) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(configs), tt.wantCount)
				}
			}
		})
	}
}

func TestCreateConfigVersion(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		user     *auth.User
		store    *mockStore
		wantCode int
	}{
		{
			name: "success",
			body: `{"config":{"interval":60},"comment":"initial"}`,
			user: &auth.User{ID: "user-1"},
			store: &mockStore{createCfgFn: func(_ context.Context, _ string, c *store.DeviceConfig) error {
				c.ID = "cfg-1"
				c.Version = 1
				return nil
			}},
			wantCode: http.StatusCreated,
		},
		{
			name:     "invalid json",
			body:     `{bad`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "empty config",
			body:     `{"comment":"no config"}`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "invalid config json",
			body:     `{"config":not valid json}`,
			store:    &mockStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "store error",
			body:     `{"config":{"interval":60}}`,
			store:    &mockStore{configErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDeviceConfigHandler(tt.store)
			req := httptest.NewRequest(http.MethodPut, "/api/devices/123/config", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			ctx := withTenant(req.Context(), "test-tenant")
			if tt.user != nil {
				ctx = withUser(ctx, tt.user)
			}
			req = req.WithContext(ctx)
			req = withChiURLParam(req, "imei", "123")
			rec := httptest.NewRecorder()
			h.CreateVersion(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}
