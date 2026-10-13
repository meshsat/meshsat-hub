package rnsnode

import (
	"time"

	"github.com/meshsat/meshsat-hub/internal/rns"
)

// LeaderLabel is the pod label the leader sets on itself so the hub-rns
// Service follows it (k8s/hub/service.yaml).
const LeaderLabel = "meshsat.net/rns-leader"

// Status is what GET /api/rns/status answers for this replica.
type Status struct {
	// Enabled is false when the node is configured off; Active is true on
	// the replica that holds leadership and runs it.
	Enabled     bool      `json:"enabled"`
	Active      bool      `json:"active"`
	TransportID string    `json:"transport_id,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	// Terms counts leadership terms this process has run the node for.
	Terms  uint64          `json:"terms"`
	Bridge *ListenerStatus `json:"bridge,omitempty"`
	Public *ListenerStatus `json:"public,omitempty"`
	// MQTT is true when the legacy Reticulum topic is attached and connected.
	MQTT  bool      `json:"mqtt"`
	Paths int       `json:"paths"`
	Links int       `json:"links"`
	Stats rns.Stats `json:"stats"`
}

// ListenerStatus describes one TCP listener.
type ListenerStatus struct {
	Addr        string     `json:"addr"`
	Connections []ConnInfo `json:"connections"`
	Stats       TCPStats   `json:"stats"`
}

// PathInfo is one path table row for GET /api/rns/paths.
type PathInfo struct {
	Dest    string    `json:"dest"`
	NextHop string    `json:"next_hop"`
	Hops    int       `json:"hops"`
	Direct  bool      `json:"direct"`
	Iface   string    `json:"iface"`
	Expires time.Time `json:"expires"`
	// AppData is the announced application data, hex, when known.
	AppData string `json:"app_data,omitempty"`
}
