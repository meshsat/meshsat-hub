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

// --- New tests for bandwidth monitoring, failover, and kernel configuration ---

func TestParseProcNetDev(t *testing.T) {
	input := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567      100    0    0    0     0          0         0  1234567      100    0    0    0     0       0          0
  eth0: 98765432    50000    0    0    0     0          0         0 12345678    25000    0    0    0     0       0          0
 wwan0: 5000000     3000    0    0    0     0          0         0  2000000     1500    0    0    0     0       0          0`

	stats := parseProcNetDev(input)

	if len(stats) != 3 {
		t.Fatalf("expected 3 interfaces, got %d", len(stats))
	}

	eth0 := stats["eth0"]
	if eth0.RxBytes != 98765432 {
		t.Errorf("eth0 RxBytes: expected 98765432, got %d", eth0.RxBytes)
	}
	if eth0.TxBytes != 12345678 {
		t.Errorf("eth0 TxBytes: expected 12345678, got %d", eth0.TxBytes)
	}

	wwan0 := stats["wwan0"]
	if wwan0.RxBytes != 5000000 {
		t.Errorf("wwan0 RxBytes: expected 5000000, got %d", wwan0.RxBytes)
	}
	if wwan0.TxBytes != 2000000 {
		t.Errorf("wwan0 TxBytes: expected 2000000, got %d", wwan0.TxBytes)
	}
}

func TestParseProcNetDev_Empty(t *testing.T) {
	stats := parseProcNetDev("")
	if len(stats) != 0 {
		t.Errorf("expected 0 interfaces, got %d", len(stats))
	}
}

func TestParseSSRTT(t *testing.T) {
	input := `ESTAB  0  0  10.0.0.1:443  192.168.1.100:50000
	 cubic rtt:25.5/3.2 mss:1460 cwnd:10`

	rttMap := parseSSRTT(input)
	if rtt, ok := rttMap["10.0.0.1"]; !ok {
		t.Error("expected RTT for 10.0.0.1")
	} else if rtt != 25 {
		t.Errorf("expected RTT 25, got %d", rtt)
	}
}

func TestParseSSRTT_MultipleConnections(t *testing.T) {
	input := `ESTAB  0  0  10.0.0.1:443  192.168.1.100:50000
	 cubic rtt:25.5/3.2 mss:1460 cwnd:10
ESTAB  0  0  192.168.1.1:443  10.0.0.50:60000
	 cubic rtt:150.0/20 mss:1460 cwnd:5`

	rttMap := parseSSRTT(input)
	if len(rttMap) != 2 {
		t.Fatalf("expected 2 RTT entries, got %d", len(rttMap))
	}
	if rttMap["10.0.0.1"] != 25 {
		t.Errorf("expected RTT 25 for 10.0.0.1, got %d", rttMap["10.0.0.1"])
	}
	if rttMap["192.168.1.1"] != 150 {
		t.Errorf("expected RTT 150 for 192.168.1.1, got %d", rttMap["192.168.1.1"])
	}
}

func TestParseSSRTT_Empty(t *testing.T) {
	rttMap := parseSSRTT("")
	if len(rttMap) != 0 {
		t.Errorf("expected 0 entries, got %d", len(rttMap))
	}
}

func TestExtractRTT(t *testing.T) {
	tests := []struct {
		line     string
		expected int64
	}{
		{"cubic rtt:25.5/3.2 mss:1460", 25},
		{"cubic rtt:100/10 mss:1460", 100},
		{"cubic rtt:0.5/0.1 mss:1460", 0},
		{"no rtt here", 0},
		{"rtt:999", 999},
	}
	for _, tt := range tests {
		result := extractRTT(tt.line)
		if result != tt.expected {
			t.Errorf("extractRTT(%q): expected %d, got %d", tt.line, tt.expected, result)
		}
	}
}

func TestApplyFailoverStrategy_PrimaryDegraded(t *testing.T) {
	subflows := []Subflow{
		{ID: "1", Interface: "eth0", Status: "active", BytesSent: 0, BytesRecv: 0, PathType: "ethernet"},
		{ID: "2", Interface: "wwan0", Status: "backup", BytesSent: 100, BytesRecv: 200, PathType: "cellular"},
	}

	applyFailoverStrategy(subflows)

	if subflows[0].Status != "degraded" {
		t.Errorf("expected primary to be degraded, got %s", subflows[0].Status)
	}
	if subflows[1].Status != "active" {
		t.Errorf("expected backup to be promoted to active, got %s", subflows[1].Status)
	}
}

func TestApplyFailoverStrategy_PrimaryHealthy(t *testing.T) {
	subflows := []Subflow{
		{ID: "1", Interface: "eth0", Status: "active", BytesSent: 1000, BytesRecv: 2000, PathType: "ethernet"},
		{ID: "2", Interface: "wwan0", Status: "backup", BytesSent: 0, BytesRecv: 0, PathType: "cellular"},
	}

	applyFailoverStrategy(subflows)

	if subflows[0].Status != "active" {
		t.Errorf("expected primary to remain active, got %s", subflows[0].Status)
	}
	if subflows[1].Status != "backup" {
		t.Errorf("expected backup to remain backup, got %s", subflows[1].Status)
	}
}

func TestApplyFailoverStrategy_Empty(t *testing.T) {
	// Should not panic on empty slice.
	applyFailoverStrategy(nil)
	applyFailoverStrategy([]Subflow{})
}

func TestApplyAggregateStrategy(t *testing.T) {
	subflows := []Subflow{
		{ID: "1", Interface: "eth0", Status: "active", BytesSent: 1000, BytesRecv: 2000},
		{ID: "2", Interface: "wwan0", Status: "active", BytesSent: 0, BytesRecv: 0},
	}

	applyAggregateStrategy(subflows)

	if subflows[0].Status != "active" {
		t.Errorf("expected eth0 to remain active, got %s", subflows[0].Status)
	}
	if subflows[1].Status != "degraded" {
		t.Errorf("expected wwan0 to be degraded, got %s", subflows[1].Status)
	}
}

func TestMonitor_EnrichBandwidth(t *testing.T) {
	m := NewMonitor(0, nil)

	// Seed previous stats.
	m.prevStats["eth0"] = BandwidthStats{Interface: "eth0", RxBytes: 1000, TxBytes: 500}

	// Validate the monitor initializes prevStats correctly.
	if m.prevStats == nil {
		t.Error("expected prevStats to be initialized")
	}
	if _, ok := m.prevStats["eth0"]; !ok {
		t.Error("expected eth0 in prevStats")
	}
}

func TestAddEndpoint_EmptyAddress(t *testing.T) {
	err := AddEndpoint(Endpoint{})
	if err == nil {
		t.Error("expected error for empty address")
	}
}

func TestRemoveEndpoint_EmptyID(t *testing.T) {
	err := RemoveEndpoint("")
	if err == nil {
		t.Error("expected error for empty ID")
	}
}

func TestAPIHandler_AddEndpoint_MissingAddress(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	body := strings.NewReader(`{"interface":"eth0","subflow":true}`)
	req := httptest.NewRequest("POST", "/api/mptcp/endpoints", body)
	w := httptest.NewRecorder()
	h.AddEndpointHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIHandler_AddEndpoint_InvalidBody(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	body := strings.NewReader(`{invalid`)
	req := httptest.NewRequest("POST", "/api/mptcp/endpoints", body)
	w := httptest.NewRecorder()
	h.AddEndpointHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIHandler_RemoveEndpoint_MissingID(t *testing.T) {
	m := NewMonitor(0, nil)
	h := NewAPIHandler(m)

	req := httptest.NewRequest("DELETE", "/api/mptcp/endpoints/", nil)
	w := httptest.NewRecorder()
	h.RemoveEndpointHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
