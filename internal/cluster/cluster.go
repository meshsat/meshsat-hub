// Package cluster provides Galera cluster health monitoring, peer discovery,
// and basic remediation actions. Each Hub instance monitors its local MariaDB
// node and exposes status via API. A coordinator view aggregates all peers.
package cluster

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NodeStatus represents the Galera status of a single MariaDB node.
type NodeStatus struct {
	// Identity
	NodeName    string `json:"node_name"`
	NodeAddress string `json:"node_address"`
	HubURL      string `json:"hub_url,omitempty"`

	// Cluster membership
	ClusterSize   int    `json:"cluster_size"`
	ClusterStatus string `json:"cluster_status"` // "Primary", "Non-primary", "Disconnected"

	// Node state
	Ready        bool   `json:"ready"`         // wsrep_ready = ON
	Connected    bool   `json:"connected"`     // wsrep_connected = ON
	StateComment string `json:"state_comment"` // "Synced", "Donor/Desynced", "Joined", "Initialized"
	StateUUID    string `json:"state_uuid"`

	// Performance
	RecvQueue        int     `json:"recv_queue"`          // local receive queue size
	SendQueue        int     `json:"send_queue"`          // local send queue size
	FlowControlPause float64 `json:"flow_control_paused"` // 0.0-1.0, fraction of time paused
	CertDepsDistance float64 `json:"cert_deps_distance"`  // avg distance between causal deps

	// Counters
	LastCommitted int64 `json:"last_committed"` // seqno of last committed transaction
	Received      int64 `json:"received"`       // total writesets received
	Replicated    int64 `json:"replicated"`     // total writesets replicated (sent)

	// Health assessment
	Healthy  bool     `json:"healthy"`
	Problems []string `json:"problems,omitempty"`

	// Timestamp
	CheckedAt string `json:"checked_at"`
}

// ClusterStatus aggregates the status of all nodes in the cluster.
type ClusterStatus struct {
	Healthy    bool         `json:"healthy"`
	NodeCount  int          `json:"node_count"`
	QuorumSize int          `json:"quorum_size"` // majority = (n/2)+1
	Nodes      []NodeStatus `json:"nodes"`
	Problems   []string     `json:"problems,omitempty"`
	CheckedAt  string       `json:"checked_at"`
}

// RemediationAction is an action that can be performed on a node.
type RemediationAction struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dangerous   bool   `json:"dangerous"`
}

// Monitor queries the local MariaDB's Galera status and aggregates peer statuses.
type Monitor struct {
	db       *sql.DB
	nodeName string
	nodeAddr string
	peers    []string // peer Hub URLs for cluster-wide view
	mu       sync.RWMutex
	last     *NodeStatus
}

// NewMonitor creates a cluster monitor.
// peers is a list of Hub URLs (e.g., ["https://192.168.192.10:8451", "https://192.168.15.10:8451"]).
func NewMonitor(db *sql.DB, nodeName, nodeAddr string, peers []string) *Monitor {
	return &Monitor{
		db:       db,
		nodeName: nodeName,
		nodeAddr: nodeAddr,
		peers:    peers,
	}
}

// LocalStatus queries the local MariaDB Galera status.
func (m *Monitor) LocalStatus(ctx context.Context) (*NodeStatus, error) {
	if m.db == nil {
		return &NodeStatus{
			NodeName:  m.nodeName,
			Healthy:   false,
			Problems:  []string{"database not configured"},
			CheckedAt: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}

	vars, err := m.queryStatusVars(ctx)
	if err != nil {
		return &NodeStatus{
			NodeName:  m.nodeName,
			Healthy:   false,
			Problems:  []string{fmt.Sprintf("query failed: %v", err)},
			CheckedAt: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}

	ns := &NodeStatus{
		NodeName:         coalesce(vars["wsrep_node_name"], m.nodeName),
		NodeAddress:      coalesce(vars["wsrep_node_address"], m.nodeAddr),
		ClusterSize:      atoi(vars["wsrep_cluster_size"]),
		ClusterStatus:    vars["wsrep_cluster_status"],
		Ready:            vars["wsrep_ready"] == "ON",
		Connected:        vars["wsrep_connected"] == "ON",
		StateComment:     vars["wsrep_local_state_comment"],
		StateUUID:        vars["wsrep_local_state_uuid"],
		RecvQueue:        atoi(vars["wsrep_local_recv_queue"]),
		SendQueue:        atoi(vars["wsrep_local_send_queue"]),
		FlowControlPause: atof(vars["wsrep_flow_control_paused"]),
		CertDepsDistance: atof(vars["wsrep_cert_deps_distance"]),
		LastCommitted:    atoi64(vars["wsrep_last_committed"]),
		Received:         atoi64(vars["wsrep_received"]),
		Replicated:       atoi64(vars["wsrep_replicated"]),
		CheckedAt:        time.Now().UTC().Format(time.RFC3339),
	}

	// Assess health
	ns.Healthy = true
	if !ns.Ready {
		ns.Healthy = false
		ns.Problems = append(ns.Problems, "node not ready (wsrep_ready=OFF)")
	}
	if !ns.Connected {
		ns.Healthy = false
		ns.Problems = append(ns.Problems, "node disconnected (wsrep_connected=OFF)")
	}
	if ns.ClusterStatus != "Primary" {
		ns.Healthy = false
		ns.Problems = append(ns.Problems, fmt.Sprintf("not in primary partition (status=%s)", ns.ClusterStatus))
	}
	if ns.StateComment != "Synced" {
		ns.Problems = append(ns.Problems, fmt.Sprintf("state: %s (expected Synced)", ns.StateComment))
		if ns.StateComment != "Donor/Desynced" {
			ns.Healthy = false
		}
	}
	if ns.FlowControlPause > 0.5 {
		ns.Problems = append(ns.Problems, fmt.Sprintf("high flow control pause: %.2f", ns.FlowControlPause))
	}
	if ns.RecvQueue > 10 {
		ns.Problems = append(ns.Problems, fmt.Sprintf("recv queue backlog: %d", ns.RecvQueue))
	}

	m.mu.Lock()
	m.last = ns
	m.mu.Unlock()

	return ns, nil
}

// ClusterWideStatus queries all peer Hubs and aggregates into a cluster view.
func (m *Monitor) ClusterWideStatus(ctx context.Context) (*ClusterStatus, error) {
	local, _ := m.LocalStatus(ctx)

	cs := &ClusterStatus{
		Healthy:   true,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Add local node
	if local != nil {
		local.HubURL = "local"
		cs.Nodes = append(cs.Nodes, *local)
	}

	// Query peers concurrently
	if len(m.peers) > 0 {
		type peerResult struct {
			url    string
			status *NodeStatus
			err    error
		}
		results := make(chan peerResult, len(m.peers))

		for _, peerURL := range m.peers {
			go func(url string) {
				ns, err := m.queryPeer(ctx, url)
				results <- peerResult{url: url, status: ns, err: err}
			}(peerURL)
		}

		for range m.peers {
			r := <-results
			if r.err != nil {
				cs.Nodes = append(cs.Nodes, NodeStatus{
					HubURL:    r.url,
					Healthy:   false,
					Problems:  []string{fmt.Sprintf("peer unreachable: %v", r.err)},
					CheckedAt: cs.CheckedAt,
				})
			} else if r.status != nil {
				r.status.HubURL = r.url
				cs.Nodes = append(cs.Nodes, *r.status)
			}
		}
	}

	// Assess cluster health
	cs.NodeCount = len(cs.Nodes)
	cs.QuorumSize = (cs.NodeCount / 2) + 1
	healthyCount := 0
	for _, n := range cs.Nodes {
		if !n.Healthy {
			cs.Healthy = false
			cs.Problems = append(cs.Problems, fmt.Sprintf("node %s unhealthy", coalesce(n.NodeName, n.HubURL)))
		} else {
			healthyCount++
		}
	}
	if healthyCount < cs.QuorumSize {
		cs.Healthy = false
		cs.Problems = append(cs.Problems, fmt.Sprintf("quorum lost: %d/%d healthy (need %d)", healthyCount, cs.NodeCount, cs.QuorumSize))
	}

	return cs, nil
}

// AvailableActions returns remediation actions for the local node.
func (m *Monitor) AvailableActions() []RemediationAction {
	return []RemediationAction{
		{ID: "resync", Name: "Force Resync", Description: "Triggers a full IST/SST resync of the local node from a donor", Dangerous: false},
		{ID: "flush", Name: "Flush Tables", Description: "Flushes all table caches and closes open tables", Dangerous: false},
		{ID: "pause", Name: "Desync Node", Description: "Temporarily removes this node from synchronous replication (for maintenance)", Dangerous: true},
		{ID: "resume", Name: "Resume Sync", Description: "Re-enables synchronous replication after desync", Dangerous: false},
		{ID: "reset_fc", Name: "Reset Flow Control", Description: "Resets flow control counters to recover from flow control stalls", Dangerous: false},
	}
}

// ExecuteAction runs a remediation action on the local MariaDB node.
func (m *Monitor) ExecuteAction(ctx context.Context, actionID string) (string, error) {
	if m.db == nil {
		return "", fmt.Errorf("database not configured")
	}

	switch actionID {
	case "resync":
		if _, err := m.db.ExecContext(ctx, "SET GLOBAL wsrep_desync=ON"); err != nil {
			return "", fmt.Errorf("desync: %w", err)
		}
		if _, err := m.db.ExecContext(ctx, "SET GLOBAL wsrep_desync=OFF"); err != nil {
			return "", fmt.Errorf("resync: %w", err)
		}
		return "Resync triggered — node will rejoin via IST/SST", nil

	case "flush":
		if _, err := m.db.ExecContext(ctx, "FLUSH TABLES"); err != nil {
			return "", fmt.Errorf("flush: %w", err)
		}
		return "Tables flushed", nil

	case "pause":
		if _, err := m.db.ExecContext(ctx, "SET GLOBAL wsrep_desync=ON"); err != nil {
			return "", fmt.Errorf("desync: %w", err)
		}
		return "Node desynced — it will not participate in synchronous replication until resumed", nil

	case "resume":
		if _, err := m.db.ExecContext(ctx, "SET GLOBAL wsrep_desync=OFF"); err != nil {
			return "", fmt.Errorf("resume: %w", err)
		}
		return "Sync resumed — node is back in synchronous replication", nil

	case "reset_fc":
		if _, err := m.db.ExecContext(ctx, "FLUSH STATUS"); err != nil {
			return "", fmt.Errorf("flush status: %w", err)
		}
		return "Flow control counters reset", nil

	default:
		return "", fmt.Errorf("unknown action: %s", actionID)
	}
}

// queryStatusVars fetches all wsrep_% status variables from MariaDB.
func (m *Monitor) queryStatusVars(ctx context.Context) (map[string]string, error) {
	rows, err := m.db.QueryContext(ctx, "SHOW STATUS LIKE 'wsrep_%'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	vars := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			continue
		}
		vars[name] = value
	}
	return vars, rows.Err()
}

// queryPeer fetches node status from a peer Hub's /api/cluster/node endpoint.
func (m *Monitor) queryPeer(ctx context.Context, hubURL string) (*NodeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := strings.TrimRight(hubURL, "/") + "/api/cluster/node"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // internal cluster peers use self-signed certs
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var ns NodeStatus
	if err := json.Unmarshal(body, &ns); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &ns, nil
}

// SetPeers updates the list of peer Hub URLs.
func (m *Monitor) SetPeers(peers []string) {
	m.mu.Lock()
	m.peers = peers
	m.mu.Unlock()
	slog.Info("cluster: peers updated", "count", len(peers))
}

// APIHandler provides HTTP endpoints for cluster management.
type APIHandler struct {
	monitor *Monitor
}

// NewAPIHandler creates a cluster management API handler.
func NewAPIHandler(monitor *Monitor) *APIHandler {
	return &APIHandler{monitor: monitor}
}

// GetNodeStatus returns the local node's Galera status.
// GET /api/cluster/node
func (h *APIHandler) GetNodeStatus(w http.ResponseWriter, r *http.Request) {
	status, _ := h.monitor.LocalStatus(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// GetClusterStatus returns the aggregated cluster status from all peers.
// GET /api/cluster/status
func (h *APIHandler) GetClusterStatus(w http.ResponseWriter, r *http.Request) {
	status, _ := h.monitor.ClusterWideStatus(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// GetActions returns available remediation actions.
// GET /api/cluster/actions
func (h *APIHandler) GetActions(w http.ResponseWriter, r *http.Request) {
	actions := h.monitor.AvailableActions()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"actions": actions})
}

// ExecuteAction runs a remediation action on the local node.
// POST /api/cluster/actions/{id}
func (h *APIHandler) ExecuteAction(w http.ResponseWriter, r *http.Request) {
	// Extract action ID from URL path
	parts := strings.Split(r.URL.Path, "/")
	actionID := parts[len(parts)-1]

	result, err := h.monitor.ExecuteAction(r.Context(), actionID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "result": result, "action": actionID})
}

// SetPeers updates the peer list dynamically.
// PUT /api/cluster/peers
func (h *APIHandler) SetPeers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peers []string `json:"peers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request"})
		return
	}
	h.monitor.SetPeers(req.Peers)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "peers": fmt.Sprintf("%d", len(req.Peers))})
}

// helpers

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func atoi(s string) int {
	var v int
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}

func atoi64(s string) int64 {
	var v int64
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}

func atof(s string) float64 {
	var v float64
	_, _ = fmt.Sscanf(s, "%f", &v)
	return v
}
