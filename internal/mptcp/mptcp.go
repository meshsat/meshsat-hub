// Package mptcp provides MPTCP (Multipath TCP) concentrator management for
// MeshSat Hub. It monitors kernel MPTCP subflows, tracks bandwidth per path,
// and provides automatic failover between satellite and cellular links.
//
// MPTCP aggregates multiple network paths (satellite, cellular, WiFi) into a
// single TCP connection, enabling transparent failover and bandwidth aggregation.
//
// Prerequisites:
//   - Linux kernel >= 5.6 with MPTCP enabled (CONFIG_MPTCP=y)
//   - sysctl net.mptcp.enabled=1
//   - ip mptcp endpoint configured for each interface
package mptcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Subflow represents a single MPTCP subflow (network path).
type Subflow struct {
	ID         string `json:"id"`
	Interface  string `json:"interface"`
	LocalAddr  string `json:"local_addr"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	PathType   string `json:"path_type"` // "satellite", "cellular", "wifi", "ethernet"
	Status     string `json:"status"`    // "active", "backup", "degraded", "down"
	BytesSent  int64  `json:"bytes_sent"`
	BytesRecv  int64  `json:"bytes_recv"`
	RTTMs      int64  `json:"rtt_ms"`
	LastSeen   string `json:"last_seen"`
}

// Status holds the overall MPTCP concentrator state.
type Status struct {
	Enabled   bool      `json:"enabled"`
	Available bool      `json:"available"` // kernel supports MPTCP
	Subflows  []Subflow `json:"subflows"`
	Strategy  string    `json:"strategy"` // "aggregate", "failover", "redundant"
	UpdatedAt string    `json:"updated_at"`
}

// Endpoint represents a configured MPTCP endpoint (ip mptcp endpoint).
type Endpoint struct {
	Address   string `json:"address"`
	Interface string `json:"interface,omitempty"`
	Signal    bool   `json:"signal"`
	Subflow   bool   `json:"subflow"`
	Backup    bool   `json:"backup"`
}

// Monitor periodically probes MPTCP kernel state and maintains a live view
// of all subflows. It publishes status updates via MQTT.
type Monitor struct {
	mu        sync.RWMutex
	status    Status
	interval  time.Duration
	publisher StatusPublisher
}

// StatusPublisher is the interface for publishing MPTCP status updates.
type StatusPublisher interface {
	PublishJSON(topic string, qos byte, retained bool, payload interface{}) error
}

// NewMonitor creates an MPTCP monitor with the given polling interval.
func NewMonitor(interval time.Duration, publisher StatusPublisher) *Monitor {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Monitor{
		interval:  interval,
		publisher: publisher,
		status: Status{
			Strategy: "failover",
		},
	}
}

// Start begins periodic MPTCP monitoring. Blocks until ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	m.probe(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("mptcp: monitor stopped")
			return
		case <-ticker.C:
			m.probe(ctx)
		}
	}
}

// GetStatus returns the current MPTCP status snapshot.
func (m *Monitor) GetStatus() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// probe reads kernel MPTCP state and updates the internal status.
func (m *Monitor) probe(_ context.Context) {
	available := isKernelMPTCPAvailable()
	enabled := isMPTCPEnabled()

	var subflows []Subflow
	if available && enabled {
		subflows = probeEndpoints()
	}

	m.mu.Lock()
	m.status = Status{
		Enabled:   enabled,
		Available: available,
		Subflows:  subflows,
		Strategy:  m.status.Strategy,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	m.mu.Unlock()

	if m.publisher != nil {
		if err := m.publisher.PublishJSON("meshsat/hub/mptcp/status", 0, true, m.status); err != nil {
			slog.Warn("mptcp: publish status failed", "error", err)
		}
	}

	slog.Debug("mptcp: probe complete", "available", available, "enabled", enabled, "subflows", len(subflows))
}

// SetStrategy sets the MPTCP path management strategy.
func (m *Monitor) SetStrategy(strategy string) error {
	switch strategy {
	case "aggregate", "failover", "redundant":
	default:
		return fmt.Errorf("unknown MPTCP strategy: %s (valid: aggregate, failover, redundant)", strategy)
	}
	m.mu.Lock()
	m.status.Strategy = strategy
	m.mu.Unlock()
	slog.Info("mptcp: strategy changed", "strategy", strategy)
	return nil
}

// isKernelMPTCPAvailable checks if the kernel supports MPTCP.
func isKernelMPTCPAvailable() bool {
	// Check for MPTCP sysctl existence
	_, err := os.ReadFile("/proc/sys/net/mptcp/enabled")
	return err == nil
}

// isMPTCPEnabled checks if MPTCP is currently enabled via sysctl.
func isMPTCPEnabled() bool {
	data, err := os.ReadFile("/proc/sys/net/mptcp/enabled")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

// probeEndpoints reads configured MPTCP endpoints from the kernel.
func probeEndpoints() []Subflow {
	// ip mptcp endpoint show (requires iproute2 >= 5.12)
	out, err := exec.Command("ip", "mptcp", "endpoint", "show").Output()
	if err != nil {
		slog.Debug("mptcp: endpoint probe failed", "error", err)
		return nil
	}

	return parseEndpoints(string(out))
}

// parseEndpoints parses `ip mptcp endpoint show` output into Subflow structs.
// Example line: "10.0.0.1 id 1 subflow dev eth0"
func parseEndpoints(output string) []Subflow {
	var subflows []Subflow
	lines := strings.Split(strings.TrimSpace(output), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		sf := Subflow{
			Status:   "active",
			LastSeen: time.Now().UTC().Format(time.RFC3339),
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		// First field is the address
		sf.LocalAddr = fields[0]

		// Parse key-value pairs
		for i := 1; i < len(fields)-1; i++ {
			switch fields[i] {
			case "id":
				sf.ID = fields[i+1]
				i++
			case "dev":
				sf.Interface = fields[i+1]
				i++
			case "backup":
				sf.Status = "backup"
			case "subflow":
				// subflow flag — already implied by default
			case "signal":
				// signal flag — endpoint advertises address
			}
		}

		sf.PathType = classifyInterface(sf.Interface)
		subflows = append(subflows, sf)
	}

	return subflows
}

// classifyInterface guesses the path type from the interface name.
func classifyInterface(iface string) string {
	switch {
	case strings.HasPrefix(iface, "wwan") || strings.HasPrefix(iface, "ppp"):
		return "cellular"
	case strings.HasPrefix(iface, "wlan") || strings.HasPrefix(iface, "wlp"):
		return "wifi"
	case strings.HasPrefix(iface, "eth") || strings.HasPrefix(iface, "enp") || strings.HasPrefix(iface, "eno"):
		return "ethernet"
	case strings.HasPrefix(iface, "sat") || strings.HasPrefix(iface, "pdn"):
		return "satellite"
	default:
		return "unknown"
	}
}

// APIHandler provides HTTP endpoints for MPTCP status and management.
type APIHandler struct {
	monitor *Monitor
}

// NewAPIHandler creates an API handler for MPTCP management.
func NewAPIHandler(monitor *Monitor) *APIHandler {
	return &APIHandler{monitor: monitor}
}

// GetStatus returns the current MPTCP concentrator status.
// GET /api/mptcp/status
func (h *APIHandler) GetStatus(w http.ResponseWriter, _ *http.Request) {
	status := h.monitor.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// SetStrategy changes the MPTCP path management strategy.
// PUT /api/mptcp/strategy
func (h *APIHandler) SetStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if err := h.monitor.SetStrategy(req.Strategy); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"strategy": req.Strategy})
}

// ListEndpoints returns the currently configured MPTCP endpoints.
// GET /api/mptcp/endpoints
func (h *APIHandler) ListEndpoints(w http.ResponseWriter, _ *http.Request) {
	status := h.monitor.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"endpoints": status.Subflows})
}
