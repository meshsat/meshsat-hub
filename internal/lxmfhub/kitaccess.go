package lxmfhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// A tenant's kits on its Private network (MESHSAT-1715; docs/bridge-hub-protocol.md
// "Reticulum access"). A kit asks on its own rns/join and the leader answers on its
// rns/access with the network name and passphrase a person would type into Sideband, or
// "none"; the leader also pushes to every bridge of the tenant when the tenant's row
// changes (a mode change, a rotation). The kit keeps one managed row that joins as a leaf.
//
// What goes out is read from the stored row, never from whether the address happens to
// be registered: a node that failed to start must not tell every kit to leave. A row
// that cannot be read sends nothing. QoS 0 and never retained, so the passphrase is not
// queued in the broker for a kit that is away; it asks again when it comes back.

// KitAccessVersion is the rns/access payload version.
const KitAccessVersion = 1

// The modes a kit is told.
const (
	KitModePrivate = "private"
	KitModeNone    = "none"
)

// kitJoinEvery bounds how often one bridge's join is answered.
const kitJoinEvery = 10 * time.Second

// KitAccess is the rns/access payload.
type KitAccess struct {
	V              int    `json:"v"`
	Mode           string `json:"mode"`
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	IFACNetname    string `json:"ifac_netname,omitempty"`
	IFACPassphrase string `json:"ifac_passphrase,omitempty"`
}

// Publisher is the bus the access goes out on.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// KitBridges says which bridges a tenant has registered.
type KitBridges interface {
	GetBridge(ctx context.Context, tenantID, bridgeID string) (*store.Bridge, error)
	ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error)
}

type kitAccess struct {
	pub     Publisher
	bridges KitBridges
	host    string
	port    int

	mu     sync.Mutex
	locks  map[string]*sync.Mutex // per tenant: one send at a time, read inside
	asked  map[string]time.Time   // per tenant/bridge: the last answered join
	pushed map[string]string      // per tenant: the last payload pushed to all
}

// SetKitAccess turns on the kits' joins; endpoint is the public host:port a
// Private network is reached on (HUB_RNS_PUBLIC_ENDPOINT).
func (s *Service) SetKitAccess(pub Publisher, bridges KitBridges, endpoint string) error {
	host, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("lxmf: kit access endpoint %q: %w", endpoint, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return fmt.Errorf("lxmf: kit access endpoint %q is not host:port", endpoint)
	}
	k := &kitAccess{pub: pub, bridges: bridges, host: host, port: port,
		locks: map[string]*sync.Mutex{}, asked: map[string]time.Time{}, pushed: map[string]string{}}
	s.mu.Lock()
	s.kits = k
	s.mu.Unlock()
	return nil
}

func (s *Service) kitCfg() *kitAccess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kits
}

func (k *kitAccess) tenantLock(tenantID string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	l := k.locks[tenantID]
	if l == nil {
		l = &sync.Mutex{}
		k.locks[tenantID] = l
	}
	return l
}

// accessFor reads what the tenant's kits are told from the stored row; ok is
// false when nothing may be said.
func (s *Service) accessFor(ctx context.Context, k *kitAccess, tenantID string) (KitAccess, bool) {
	none := KitAccess{V: KitAccessVersion, Mode: KitModeNone}
	id, err := s.LoadIdentity(ctx, tenantID)
	switch {
	case errors.Is(err, ErrNoIdentity):
		return none, true
	case err != nil:
		return KitAccess{}, false
	case !id.Enabled || id.AccessMode != AccessPrivate:
		return none, true
	case id.IFACNetname == "" || id.IFACPassphrase == "":
		return KitAccess{}, false
	}
	return KitAccess{V: KitAccessVersion, Mode: KitModePrivate, Host: k.host, Port: k.port,
		IFACNetname: id.IFACNetname, IFACPassphrase: id.IFACPassphrase}, true
}

// sendKitAccess reads the tenant's access and publishes it to the bridges
// named, under the tenant's send lock, so two sends never cross with an older
// row read last. all marks a push to every bridge, which is skipped when
// nothing changed since the last one.
func (s *Service) sendKitAccess(ctx context.Context, k *kitAccess, tenantID string, bridgeIDs []string, all bool) (int, KitAccess) {
	lk := k.tenantLock(tenantID)
	lk.Lock()
	defer lk.Unlock()
	a, ok := s.accessFor(ctx, k, tenantID)
	if !ok {
		return 0, a
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return 0, a
	}
	if all {
		k.mu.Lock()
		same := k.pushed[tenantID] == string(payload)
		k.mu.Unlock()
		if same {
			return 0, a
		}
	}
	sent := 0
	for _, b := range bridgeIDs {
		if err := k.pub.Publish(hubmqtt.TopicRNSAccessFor(tenantID, b), 0, false, payload); err != nil {
			slog.Warn("lxmf: access not sent to a kit", "tenant", tenantID, "bridge_id", b, "error", err)
			continue
		}
		sent++
	}
	if all && sent == len(bridgeIDs) {
		k.mu.Lock()
		k.pushed[tenantID] = string(payload)
		k.mu.Unlock()
	}
	return sent, a
}

// pushKitAccess tells every bridge of the tenant what its row says now; the
// leader calls it after the row changed.
func (s *Service) pushKitAccess(ctx context.Context, tenantID string) {
	k := s.kitCfg()
	if k == nil || !s.Active() {
		return
	}
	bs, err := k.bridges.ListBridges(ctx, tenantID)
	if err != nil {
		slog.Warn("lxmf: the tenant's bridges are unreadable, kits not told", "tenant", tenantID, "error", err)
		return
	}
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		if b != nil && b.BridgeID != "" {
			ids = append(ids, b.BridgeID)
		}
	}
	if len(ids) == 0 {
		return
	}
	n, a := s.sendKitAccess(ctx, k, tenantID, ids, true)
	if n == 0 {
		return
	}
	slog.Info("lxmf: kits told the workspace's Reticulum access", "tenant", tenantID, "mode", a.Mode, "bridges", n)
	if a.Mode == KitModePrivate {
		s.auditLog(ctx, tenantID, "lxmf_access_sent_to_kits", "hub",
			fmt.Sprintf("network name %s sent to %d bridge(s)", a.IFACNetname, n))
	}
}

// HandleJoin is the bus handler for rns/join on every replica; the leader
// answers. The work runs off the bus goroutine: it reads the store and publishes.
func (s *Service) HandleJoin(topic string, _ []byte) {
	go s.handleJoin(topic)
}

func (s *Service) handleJoin(topic string) {
	k := s.kitCfg()
	if k == nil || !s.Active() {
		return
	}
	tenantID, bridgeID, ok := hubmqtt.ParseRNSJoinTopic(topic)
	if !ok {
		return
	}
	now := s.now()
	key := tenantID + "/" + bridgeID
	k.mu.Lock()
	if last, seen := k.asked[key]; seen && now.Sub(last) < kitJoinEvery {
		k.mu.Unlock()
		return
	}
	k.asked[key] = now
	for kk, t := range k.asked {
		if now.Sub(t) > time.Hour {
			delete(k.asked, kk)
		}
	}
	k.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The store files the bridge under the topic's tenant, or nothing is said:
	// the tenant is the namespace the broker enforced, and the bridge must be
	// registered there now. No cache and no fallback tenant.
	b, err := k.bridges.GetBridge(ctx, tenantID, bridgeID)
	if err != nil || b == nil || b.TenantID != tenantID || b.BridgeID != bridgeID {
		slog.Info("lxmf: a join from a bridge not registered to the topic's tenant, not answered", "tenant", tenantID, "bridge_id", bridgeID)
		return
	}
	n, a := s.sendKitAccess(ctx, k, tenantID, []string{bridgeID}, false)
	slog.Info("lxmf: a kit's join answered", "tenant", tenantID, "bridge_id", bridgeID, "mode", a.Mode, "sent", n == 1)
}
