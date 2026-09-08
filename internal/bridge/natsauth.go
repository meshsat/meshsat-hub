package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// NATS per-bridge authentication (MESHSAT-864 MR 21).
//
// The Hub renders the NATS `authorization` block into the Kubernetes Secret
// meshsat-nats-auth (key users.conf), which the NATS pod includes from its
// config and reloads on change. It replaces the Mosquitto passwd/ACL files:
// one NATS user per bridge that has credentials, its bcrypt hash straight
// from the bridges table, and permissions confined to the bridge's own
// subtree and the device topics of its tenant. The shared `meshsat` user
// stays for the Hub itself; its password is still resolved from the pod
// environment by nats-server, so the rendered file never carries it.

// SharedNATSUser is the Hub's own NATS/MQTT user.
const SharedNATSUser = "meshsat"

// natsSubject converts a topic-shaped string (slashes) into a NATS subject.
func natsSubject(topic string) string {
	return strings.ReplaceAll(topic, "/", ".")
}

// NATSPermissions returns the publish and subscribe allow lists for a bridge
// whose tenant namespace is ns ("meshsat" or "meshsat/{tenant}").
func NATSPermissions(ns, bridgeID string) (publish, subscribe []string) {
	n := natsSubject(ns)
	own := n + ".bridge." + bridgeID + ".>"
	publish = []string{
		own,
		n + ".*.position", n + ".*.telemetry", n + ".*.sos", n + ".*.health", n + ".*.signal",
		n + ".*.mo.>", n + ".*.status.>", n + ".*.sms.>", n + ".*.config.current",
	}
	subscribe = []string{
		own,
		n + ".*.mt.>", n + ".*.config.>",
		"meshsat.broadcast.>", "meshsat.hub.>",
		"$MQTT.sub.>", // nats-server's own subject for QoS 1 MQTT subscriptions
	}
	return publish, subscribe
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = `"` + s + `"`
	}
	return strings.Join(q, ", ")
}

// validIdent restricts what ends up unquoted-adjacent in the config: bridge
// IDs and usernames are hostnames/handles; anything else is skipped.
func validIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// RenderNATSUsers renders the authorization block. namespace maps a tenant
// ID to its topic root (hubmqtt.Namespace). Output is deterministic (sorted
// by username) so unchanged fleets produce byte-identical files.
func RenderNATSUsers(bridges []*store.Bridge, namespace func(tenantID string) string) []byte {
	type user struct{ name, hash, ns, id string }
	users := make([]user, 0, len(bridges))
	for _, b := range bridges {
		if b == nil || b.MQTTUsername == "" || b.MQTTPasswordHash == "" || b.MQTTUsername == SharedNATSUser {
			continue
		}
		if !validIdent(b.MQTTUsername) || !validIdent(b.BridgeID) || !strings.HasPrefix(b.MQTTPasswordHash, "$2") || strings.ContainsAny(b.MQTTPasswordHash, "\"\n\\") {
			slog.Warn("nats-auth: skipping bridge with unusable credentials", "bridge", b.BridgeID)
			continue
		}
		users = append(users, user{name: b.MQTTUsername, hash: b.MQTTPasswordHash, ns: namespace(b.TenantID), id: b.BridgeID})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].name < users[j].name })

	var w strings.Builder
	w.WriteString("# Rendered by meshsat-hub (internal/bridge/natsauth.go); do not edit by hand.\n")
	fmt.Fprintf(&w, "# %d bridge user(s). The shared user's password comes from the pod environment.\n", len(users))
	w.WriteString("authorization {\n  users = [\n")
	w.WriteString("    { user: " + SharedNATSUser + ", password: $NATS_MQTT_PASSWORD }\n")
	for _, u := range users {
		pub, sub := NATSPermissions(u.ns, u.id)
		fmt.Fprintf(&w, "    { user: \"%s\", password: \"%s\", permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }\n",
			u.name, u.hash, quoteList(pub), quoteList(sub))
	}
	w.WriteString("  ]\n}\n")
	return []byte(w.String())
}

// Resyncer is what the API handlers call after credentials change.
type Resyncer interface {
	Trigger()
}

// NATSAuthSyncer keeps the rendered users file in the Secret.
type NATSAuthSyncer struct {
	store     store.Store
	writer    *CASecretWriter
	namespace func(string) string
	interval  time.Duration
	kick      chan struct{}
	mu        sync.Mutex
	lastErr   error
	synced    time.Time
	bridges   int
}

// NewNATSAuthSyncer wires the store to the Secret writer. interval bounds the
// periodic resync (0 = every 5 minutes).
func NewNATSAuthSyncer(s store.Store, w *CASecretWriter, namespace func(string) string, interval time.Duration) *NATSAuthSyncer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &NATSAuthSyncer{store: s, writer: w, namespace: namespace, interval: interval, kick: make(chan struct{}, 1)}
}

// Trigger asks for a resync soon; never blocks.
func (n *NATSAuthSyncer) Trigger() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// Sync renders and writes once.
func (n *NATSAuthSyncer) Sync(ctx context.Context) error {
	bridges, err := n.store.ListBridgesWithCredentials(ctx)
	if err == nil {
		err = n.writer.Sync(ctx, RenderNATSUsers(bridges, n.namespace))
	}
	n.mu.Lock()
	n.lastErr = err
	if err == nil {
		n.synced = time.Now()
		n.bridges = len(bridges)
	}
	n.mu.Unlock()
	if err != nil {
		slog.Warn("nats-auth: sync failed", "error", err)
	}
	return err
}

// Run syncs at start, on every Trigger and every interval until ctx ends.
func (n *NATSAuthSyncer) Run(ctx context.Context) {
	_ = n.Sync(ctx)
	t := time.NewTicker(n.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.kick:
		case <-t.C:
		}
		_ = n.Sync(ctx)
	}
}

// LastError reports the most recent outcome (nil = the Secret is current).
func (n *NATSAuthSyncer) LastError() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastErr
}
