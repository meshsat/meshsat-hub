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
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// validInterface matches alphanumeric interface names (letters, digits, underscore, hyphen).
var validInterface = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validateEndpointID rejects non-numeric endpoint IDs to prevent command injection.
func validateEndpointID(id string) error {
	for _, c := range id {
		if c < '0' || c > '9' {
			return fmt.Errorf("endpoint id must be numeric, got %q", id)
		}
	}
	return nil
}

// validateAddress checks that an address is a valid IP (v4 or v6).
func validateAddress(addr string) error {
	if net.ParseIP(addr) == nil {
		return fmt.Errorf("invalid IP address: %q", addr)
	}
	return nil
}

// validateInterfaceName checks that an interface name contains only safe characters.
func validateInterfaceName(iface string) error {
	if !validInterface.MatchString(iface) {
		return fmt.Errorf("invalid interface name: %q (must be alphanumeric, underscore, or hyphen)", iface)
	}
	return nil
}

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

// BandwidthStats holds per-interface bandwidth counters read from /proc/net/dev.
type BandwidthStats struct {
	Interface string `json:"interface"`
	RxBytes   int64  `json:"rx_bytes"`
	TxBytes   int64  `json:"tx_bytes"`
}

// Monitor periodically probes MPTCP kernel state and maintains a live view
// of all subflows. It publishes status updates via MQTT.
type Monitor struct {
	mu        sync.RWMutex
	status    Status
	interval  time.Duration
	publisher StatusPublisher

	// prevStats holds previous bandwidth readings for delta computation.
	prevStats map[string]BandwidthStats
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
		prevStats: make(map[string]BandwidthStats),
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
		m.enrichBandwidth(subflows)
		m.applyFailover(subflows)
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

// enrichBandwidth reads /proc/net/dev to populate per-subflow bandwidth counters.
func (m *Monitor) enrichBandwidth(subflows []Subflow) {
	stats := readProcNetDev()
	for i := range subflows {
		iface := subflows[i].Interface
		if iface == "" {
			continue
		}
		cur, ok := stats[iface]
		if !ok {
			continue
		}
		// Compute delta from previous reading.
		if prev, havePrev := m.prevStats[iface]; havePrev {
			subflows[i].BytesRecv = cur.RxBytes - prev.RxBytes
			subflows[i].BytesSent = cur.TxBytes - prev.TxBytes
			// Guard against counter reset.
			if subflows[i].BytesRecv < 0 {
				subflows[i].BytesRecv = cur.RxBytes
			}
			if subflows[i].BytesSent < 0 {
				subflows[i].BytesSent = cur.TxBytes
			}
		}
		m.prevStats[iface] = cur
	}

	enrichRTT(subflows)
}

// readProcNetDev parses /proc/net/dev for per-interface byte counters.
func readProcNetDev() map[string]BandwidthStats {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		slog.Debug("mptcp: cannot read /proc/net/dev", "error", err)
		return nil
	}
	return parseProcNetDev(string(data))
}

// parseProcNetDev parses the contents of /proc/net/dev.
// Format: "iface: rx_bytes rx_packets ... tx_bytes tx_packets ..."
func parseProcNetDev(content string) map[string]BandwidthStats {
	result := make(map[string]BandwidthStats)
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		// Skip header lines (contain "|").
		if strings.Contains(line, "|") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}
		rxBytes, _ := strconv.ParseInt(fields[0], 10, 64)
		txBytes, _ := strconv.ParseInt(fields[8], 10, 64)
		result[iface] = BandwidthStats{
			Interface: iface,
			RxBytes:   rxBytes,
			TxBytes:   txBytes,
		}
	}
	return result
}

// enrichRTT reads RTT estimates from `ss --no-header -i -M` output.
func enrichRTT(subflows []Subflow) {
	out, err := exec.Command("ss", "--no-header", "-i", "-M").Output()
	if err != nil {
		slog.Debug("mptcp: ss probe failed", "error", err)
		return
	}
	rttByAddr := parseSSRTT(string(out))
	for i := range subflows {
		if rtt, ok := rttByAddr[subflows[i].LocalAddr]; ok {
			subflows[i].RTTMs = rtt
		}
	}
}

// parseSSRTT extracts RTT values from `ss -i -M` output.
// The output alternates: connection line, then info line with "rtt:X/Y".
// Connection lines contain local_addr:port and we match on address.
func parseSSRTT(output string) map[string]int64 {
	result := make(map[string]int64)
	lines := strings.Split(output, "\n")

	var currentAddr string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Info lines start with whitespace or contain "rtt:".
		if strings.Contains(trimmed, "rtt:") {
			rtt := extractRTT(trimmed)
			if currentAddr != "" && rtt > 0 {
				result[currentAddr] = rtt
			}
			currentAddr = ""
			continue
		}

		// Connection line: extract local address (before port).
		fields := strings.Fields(trimmed)
		for _, f := range fields {
			if idx := strings.LastIndex(f, ":"); idx > 0 {
				addr := f[:idx]
				// Remove brackets for IPv6.
				addr = strings.TrimPrefix(strings.TrimSuffix(addr, "]"), "[")
				if addr != "" && addr != "*" {
					currentAddr = addr
					break
				}
			}
		}
	}
	return result
}

// extractRTT finds "rtt:X" or "rtt:X/Y" in a line and returns X as ms.
func extractRTT(line string) int64 {
	idx := strings.Index(line, "rtt:")
	if idx < 0 {
		return 0
	}
	rest := line[idx+4:]
	// rtt value ends at "/" or whitespace.
	end := strings.IndexAny(rest, "/ \t")
	if end < 0 {
		end = len(rest)
	}
	val := rest[:end]
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// applyFailover evaluates subflow health and adjusts status based on the
// configured strategy. In "failover" mode, if the primary path (lowest ID,
// non-backup) shows zero bandwidth over the monitoring interval, backup
// paths are promoted to "active". In "aggregate" mode, paths with zero
// bandwidth are marked "degraded".
func (m *Monitor) applyFailover(subflows []Subflow) {
	m.mu.RLock()
	strategy := m.status.Strategy
	m.mu.RUnlock()

	switch strategy {
	case "failover":
		applyFailoverStrategy(subflows)
	case "aggregate":
		applyAggregateStrategy(subflows)
	case "redundant":
		// Redundant mode keeps all paths active — no status changes.
	}
}

// applyFailoverStrategy promotes backup paths when the primary is degraded.
func applyFailoverStrategy(subflows []Subflow) {
	if len(subflows) == 0 {
		return
	}

	// Find primary (first non-backup subflow).
	primaryIdx := -1
	for i, sf := range subflows {
		if sf.Status == "active" {
			primaryIdx = i
			break
		}
	}
	if primaryIdx < 0 {
		return
	}

	primary := &subflows[primaryIdx]

	// If primary has zero bandwidth in both directions, mark degraded
	// and promote first backup.
	if primary.BytesSent == 0 && primary.BytesRecv == 0 {
		primary.Status = "degraded"
		slog.Warn("mptcp: primary path degraded, promoting backup",
			"interface", primary.Interface, "path_type", primary.PathType)

		for i := range subflows {
			if i != primaryIdx && subflows[i].Status == "backup" {
				subflows[i].Status = "active"
				slog.Info("mptcp: backup promoted to active",
					"interface", subflows[i].Interface, "path_type", subflows[i].PathType)
				break
			}
		}
	}
}

// applyAggregateStrategy marks zero-bandwidth paths as degraded.
func applyAggregateStrategy(subflows []Subflow) {
	for i := range subflows {
		if subflows[i].Status == "active" && subflows[i].BytesSent == 0 && subflows[i].BytesRecv == 0 {
			subflows[i].Status = "degraded"
		}
	}
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

// AddEndpoint configures a new MPTCP endpoint in the kernel.
func AddEndpoint(ep Endpoint) error {
	if ep.Address == "" {
		return fmt.Errorf("address is required")
	}
	if err := validateAddress(ep.Address); err != nil {
		return err
	}
	if ep.Interface != "" {
		if err := validateInterfaceName(ep.Interface); err != nil {
			return err
		}
	}

	args := []string{"mptcp", "endpoint", "add", ep.Address}
	if ep.Interface != "" {
		args = append(args, "dev", ep.Interface)
	}
	if ep.Signal {
		args = append(args, "signal")
	}
	if ep.Subflow {
		args = append(args, "subflow")
	}
	if ep.Backup {
		args = append(args, "backup")
	}

	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip mptcp endpoint add: %s: %w", strings.TrimSpace(string(out)), err)
	}
	slog.Info("mptcp: endpoint added", "address", ep.Address, "interface", ep.Interface, "backup", ep.Backup)
	return nil
}

// RemoveEndpoint removes an MPTCP endpoint by ID.
func RemoveEndpoint(id string) error {
	if id == "" {
		return fmt.Errorf("endpoint id is required")
	}
	if err := validateEndpointID(id); err != nil {
		return err
	}

	out, err := exec.Command("ip", "mptcp", "endpoint", "delete", "id", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip mptcp endpoint delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	slog.Info("mptcp: endpoint removed", "id", id)
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
//
//	@Summary		Get MPTCP concentrator status
//	@Description	Returns the current state of the MPTCP concentrator including kernel availability, enabled status, active subflows with bandwidth metrics, and the current path management strategy.
//	@Tags			mptcp
//	@Produce		json
//	@Success		200	{object}	Status
//	@Router			/api/mptcp/status [get]
func (h *APIHandler) GetStatus(w http.ResponseWriter, _ *http.Request) {
	status := h.monitor.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// SetStrategy changes the MPTCP path management strategy.
//
//	@Summary		Set MPTCP path management strategy
//	@Description	Changes the strategy used for managing multiple network paths. "aggregate" uses all paths simultaneously for maximum throughput. "failover" uses the primary path and promotes a backup if the primary degrades. "redundant" sends traffic on all paths for maximum reliability.
//	@Tags			mptcp
//	@Accept			json
//	@Produce		json
//	@Param			body	body		object{strategy=string}	true	"Strategy (aggregate, failover, redundant)"
//	@Success		200		{object}	object{strategy=string}
//	@Failure		400		{object}	object{error=string}
//	@Router			/api/mptcp/strategy [put]
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
//
//	@Summary		List MPTCP endpoints
//	@Description	Returns all configured MPTCP endpoints with their current bandwidth metrics and status.
//	@Tags			mptcp
//	@Produce		json
//	@Success		200	{object}	object{endpoints=[]Subflow}
//	@Router			/api/mptcp/endpoints [get]
func (h *APIHandler) ListEndpoints(w http.ResponseWriter, _ *http.Request) {
	status := h.monitor.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"endpoints": status.Subflows})
}

// AddEndpointHandler adds a new MPTCP endpoint via the kernel.
//
//	@Summary		Add MPTCP endpoint
//	@Description	Configures a new MPTCP endpoint in the kernel for the specified address and interface.
//	@Tags			mptcp
//	@Accept			json
//	@Produce		json
//	@Param			body	body		Endpoint	true	"Endpoint configuration"
//	@Success		201		{object}	Endpoint
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/mptcp/endpoints [post]
func (h *APIHandler) AddEndpointHandler(w http.ResponseWriter, r *http.Request) {
	var ep Endpoint
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&ep); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if ep.Address == "" {
		http.Error(w, `{"error":"address is required"}`, http.StatusBadRequest)
		return
	}
	if err := AddEndpoint(ep); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ep)
}

// RemoveEndpointHandler removes an MPTCP endpoint by ID.
//
//	@Summary		Remove MPTCP endpoint
//	@Description	Removes a configured MPTCP endpoint from the kernel by its ID.
//	@Tags			mptcp
//	@Produce		json
//	@Param			id	path		string	true	"Endpoint ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/mptcp/endpoints/{id} [delete]
func (h *APIHandler) RemoveEndpointHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"endpoint id is required"}`, http.StatusBadRequest)
		return
	}
	if err := RemoveEndpoint(id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
