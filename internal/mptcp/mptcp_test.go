package mptcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseEndpoints_Empty(t *testing.T) {
	subflows := parseEndpoints("")
	if len(subflows) != 0 {
		t.Errorf("expected 0 subflows, got %d", len(subflows))
	}
}

func TestParseEndpoints_SingleLine(t *testing.T) {
	input := "10.0.0.1 id 1 subflow dev eth0"
	subflows := parseEndpoints(input)

	if len(subflows) != 1 {
		t.Fatalf("expected 1 subflow, got %d", len(subflows))
	}
	sf := subflows[0]
	if sf.LocalAddr != "10.0.0.1" {
		t.Errorf("expected addr 10.0.0.1, got %s", sf.LocalAddr)
	}
	if sf.ID != "1" {
		t.Errorf("expected id 1, got %s", sf.ID)
	}
	if sf.Interface != "eth0" {
		t.Errorf("expected dev eth0, got %s", sf.Interface)
	}
	if sf.PathType != "ethernet" {
		t.Errorf("expected path type ethernet, got %s", sf.PathType)
	}
	if sf.Status != "active" {
		t.Errorf("expected status active, got %s", sf.Status)
	}
}

func TestParseEndpoints_BackupFlag(t *testing.T) {
	input := "192.168.1.1 id 2 subflow backup dev wwan0"
	subflows := parseEndpoints(input)

	if len(subflows) != 1 {
		t.Fatalf("expected 1 subflow, got %d", len(subflows))
	}
	sf := subflows[0]
	if sf.Status != "backup" {
		t.Errorf("expected status backup, got %s", sf.Status)
	}
	if sf.PathType != "cellular" {
		t.Errorf("expected path type cellular, got %s", sf.PathType)
	}
}

func TestParseEndpoints_MultiLine(t *testing.T) {
	input := `10.0.0.1 id 1 subflow dev eth0
192.168.1.1 id 2 subflow backup dev wwan0
10.0.0.2 id 3 signal dev wlan0`

	subflows := parseEndpoints(input)
	if len(subflows) != 3 {
		t.Fatalf("expected 3 subflows, got %d", len(subflows))
	}

	// Verify interface classification
	types := map[string]string{
		"eth0":  "ethernet",
		"wwan0": "cellular",
		"wlan0": "wifi",
	}
	for _, sf := range subflows {
		expected := types[sf.Interface]
		if sf.PathType != expected {
			t.Errorf("interface %s: expected type %s, got %s", sf.Interface, expected, sf.PathType)
		}
	}
}

func TestClassifyInterface(t *testing.T) {
	tests := []struct {
		iface    string
		expected string
	}{
		{"eth0", "ethernet"},
		{"enp0s3", "ethernet"},
		{"eno1", "ethernet"},
		{"wlan0", "wifi"},
		{"wlp2s0", "wifi"},
		{"wwan0", "cellular"},
		{"ppp0", "cellular"},
		{"sat0", "satellite"},
		{"pdn0", "satellite"},
		{"lo", "unknown"},
		{"tun0", "unknown"},
	}

	for _, tt := range tests {
		result := classifyInterface(tt.iface)
		if result != tt.expected {
			t.Errorf("classifyInterface(%s): expected %s, got %s", tt.iface, tt.expected, result)
		}
	}
}

func TestMonitor_SetStrategy(t *testing.T) {
	m := NewMonitor(0, nil)

	// Valid strategies
	for _, s := range []string{"aggregate", "failover", "redundant"} {
		if err := m.SetStrategy(s); err != nil {
			t.Errorf("SetStrategy(%s): unexpected error: %v", s, err)
		}
		status := m.GetStatus()
		if status.Strategy != s {
			t.Errorf("expected strategy %s, got %s", s, status.Strategy)
		}
	}

	// Invalid strategy
	if err := m.SetStrategy("invalid"); err == nil {
		t.Error("expected error for invalid strategy")
	}
}

func TestMonitor_GetStatus_Defaults(t *testing.T) {
	m := NewMonitor(0, nil)
	status := m.GetStatus()

	if status.Enabled {
		t.Error("expected disabled by default")
	}
	if status.Strategy != "failover" {
		t.Errorf("expected default strategy failover, got %s", status.Strategy)
	}
}

func TestMonitor_Probe_WithPublisher(t *testing.T) {
	pub := &testPublisher{}
	m := NewMonitor(0, pub)
	// Probe won't find MPTCP on this host but should not panic
	m.probe(context.Background())
	status := m.GetStatus()
	if status.UpdatedAt == "" {
		t.Error("expected UpdatedAt to be set after probe")
	}
	if len(pub.published) != 1 {
		t.Errorf("expected 1 publish, got %d", len(pub.published))
	}
}

type testPublisher struct {
	published []interface{}
}

func (p *testPublisher) PublishJSON(_ string, _ byte, _ bool, payload interface{}) error {
	p.published = append(p.published, payload)
	return nil
}

func TestAPIHandler_GetStatus(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	req := httptest.NewRequest("GET", "/api/mptcp/status", nil)
	w := httptest.NewRecorder()
	h.GetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var status Status
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if status.Strategy != "failover" {
		t.Errorf("expected failover strategy, got %s", status.Strategy)
	}
}

func TestAPIHandler_SetStrategy(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	body := strings.NewReader(`{"strategy":"aggregate"}`)
	req := httptest.NewRequest("PUT", "/api/mptcp/strategy", body)
	w := httptest.NewRecorder()
	h.SetStrategy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	status := m.GetStatus()
	if status.Strategy != "aggregate" {
		t.Errorf("expected aggregate, got %s", status.Strategy)
	}
}

func TestAPIHandler_SetStrategy_Invalid(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	body := strings.NewReader(`{"strategy":"invalid"}`)
	req := httptest.NewRequest("PUT", "/api/mptcp/strategy", body)
	w := httptest.NewRecorder()
	h.SetStrategy(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIHandler_ListEndpoints(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	req := httptest.NewRequest("GET", "/api/mptcp/endpoints", nil)
	w := httptest.NewRecorder()
	h.ListEndpoints(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["endpoints"]; !ok {
		t.Error("expected 'endpoints' key in response")
	}
}
