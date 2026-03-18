package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func TestListPositions(t *testing.T) {
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
			store:     &mockStore{positions: nil},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:  "returns positions",
			query: "",
			store: &mockStore{positions: []store.Position{
				{ID: "1", DeviceIMEI: "123", Lat: 52.16, Lon: 4.50, Source: "gps", CreatedAt: now},
			}},
			wantCode:  http.StatusOK,
			wantCount: 1,
		},
		{
			name:      "with limit param",
			query:     "?limit=5",
			store:     &mockStore{positions: []store.Position{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:      "invalid limit ignored",
			query:     "?limit=-1",
			store:     &mockStore{positions: []store.Position{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:     "store error",
			query:    "",
			store:    &mockStore{positionErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewPositionHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/123/positions"+tt.query, map[string]string{"imei": "123"})
			h.ListPositions(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var resp paginatedPositions
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(resp.Positions) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(resp.Positions), tt.wantCount)
				}
			}
		})
	}
}

func TestLatestPosition(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		store    *mockStore
		wantCode int
	}{
		{
			name: "found",
			store: &mockStore{position: &store.Position{
				ID: "1", DeviceIMEI: "123", Lat: 52.16, Lon: 4.50, Source: "gps", CreatedAt: now,
			}},
			wantCode: http.StatusOK,
		},
		{
			name:     "no position data",
			store:    &mockStore{position: nil},
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewPositionHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/devices/123/position", map[string]string{"imei": "123"})
			h.LatestPosition(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestAllLatestPositions(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		store     *mockStore
		wantCode  int
		wantCount int
	}{
		{
			name:      "no devices",
			store:     &mockStore{devices: []store.Device{}},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name: "devices with positions",
			store: &mockStore{
				devices: []store.Device{
					{IMEI: "123", Label: "A", Type: "rockblock"},
				},
				position: &store.Position{Lat: 52.16, Lon: 4.50, Source: "gps", CreatedAt: now},
			},
			wantCode:  http.StatusOK,
			wantCount: 1,
		},
		{
			name: "devices without positions skipped",
			store: &mockStore{
				devices:  []store.Device{{IMEI: "123", Label: "A", Type: "rockblock"}},
				position: nil,
			},
			wantCode:  http.StatusOK,
			wantCount: 0,
		},
		{
			name:     "store error on list devices",
			store:    &mockStore{deviceErr: fmt.Errorf("db error")},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewPositionHandler(tt.store)
			req, rec := newTestRequest(http.MethodGet, "/api/positions/latest", nil)
			h.AllLatestPositions(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				var positions []json.RawMessage
				if err := json.NewDecoder(rec.Body).Decode(&positions); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(positions) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(positions), tt.wantCount)
				}
			}
		})
	}
}
