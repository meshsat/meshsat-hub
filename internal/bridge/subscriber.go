// Package bridge implements the Hub-side MQTT subscriber for bridge lifecycle events.
// It listens for birth/death/health messages from field bridges and auto-provisions
// bridge and device records in the Hub store.
package bridge

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/sos"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ReticulumRouter is the subset of the Reticulum routing table needed by the
// bridge subscriber to inject/remove routes when bridges come online/offline.
type ReticulumRouter interface {
	InjectRoute(destHashHex string, signingPub []byte, iface string, hops int) bool
	RemoveHex(destHex string) bool
	RefreshRoute(destHashHex string) bool
}

// HeMBReassembler is the interface for the HeMB reassembly buffer used by the
// bridge subscriber to reassemble bonded traffic from bridges.
type HeMBReassembler interface {
	AddSymbol(streamID uint8, bearerIndex uint8, sym protocol.HeMBCodedSymbol) ([]byte, error)
	Reap() int
	Stats() protocol.HeMBReassemblyStats
}

// Subscriber listens on bridge lifecycle MQTT topics and auto-provisions
// bridge and device records in the Hub store.
type Subscriber struct {
	mqtt               bus.MessageBus
	store              store.Store
	tenants            *tenancy.Resolver
	staleThreshold     time.Duration // birth messages older than this are treated as retained replays
	retRouter          ReticulumRouter
	caCertPool         *x509.CertPool // bridge CA for birth signature verification
	birthSignatureMode string         // "warn" (default) or "enforce"
	hembReassembler    HeMBReassembler
	quota              QuotaChecker  // nil = no subscription ceiling
	leader             LeaderChecker // nil = every replica writes health reports
	sosV2              *bool         // nil = no hub/caps; else whether caps say sos_v2 (C10)

	// Births not yet written in full (HeMB-NG S3-2), and the loop that
	// writes them again; see egress_pending.go.
	pendingOnce   sync.Once
	egressPending *pendingBirths
	stopOnce      sync.Once
	stopEgress    chan struct{}
}

// SetSOSV2 makes every fresh birth answered on the bridge's retained hub/caps, saying
// whether this Hub takes the C10 SOS frames and events (sos_v2). A bridge sends them only
// while its Hub says so.
func (s *Subscriber) SetSOSV2(on bool) {
	s.sosV2 = &on
}

// LeaderChecker is the one question this package asks the leader election:
// whether this replica holds the Lease. Declared here so the package does
// not import internal/leader for a single method.
type LeaderChecker interface {
	IsLeader() bool
}

// SetLeader makes the health-report row write a leader-only action
// (MESHSAT-1155). Both replicas receive every health report; with both
// writing, two updates to the same bridges row queued behind each other's
// synchronous commits and were the only slow statements on the platform.
// A report is idempotent and the next one is minutes away, so a report that
// lands during a leadership handover is simply the next one's job. The
// in-memory work (the Reticulum route refresh) still runs on every replica.
func (s *Subscriber) SetLeader(l LeaderChecker) {
	s.leader = l
}

// NewSubscriber creates a new bridge MQTT subscriber.
func NewSubscriber(mqtt bus.MessageBus, st store.Store, tenants *tenancy.Resolver) *Subscriber {
	if tenants == nil {
		tenants = tenancy.NewResolver(st, store.DefaultTenantID, 30*time.Second)
	}
	return &Subscriber{
		mqtt:           mqtt,
		store:          st,
		tenants:        tenants,
		staleThreshold: 5 * time.Minute,
	}
}

// QuotaChecker is the one question this package asks internal/quota, declared
// here rather than imported so that internal/bridge -- which is on an ingest
// path -- carries no dependency on the billing code at all. The invariant test
// in internal/quota asserts exactly that: nothing that receives field traffic
// may reach the subscription ceiling.
type QuotaChecker interface {
	AllowAnother(ctx context.Context, tenantID string) (bool, string)
}

// SetQuota attaches the subscription device ceiling. It gates auto-provisioning
// a device the Hub has never seen, and nothing else: the message that carried
// the birth is still processed, the bridge stays online, and every device
// already registered keeps reporting. See internal/quota.
func (s *Subscriber) SetQuota(q QuotaChecker) {
	if q != nil {
		s.quota = q
	}
}

// allowAnother is the nil-safe form: with no quota configured, every
// registration is allowed.
func (s *Subscriber) allowAnother(ctx context.Context, tenantID string) (bool, string) {
	if s.quota == nil {
		return true, ""
	}
	return s.quota.AllowAnother(ctx, tenantID)
}

// SetStaleThreshold configures how old a birth timestamp can be before it is
// treated as a retained replay rather than a fresh connection.
func (s *Subscriber) SetStaleThreshold(d time.Duration) {
	s.staleThreshold = d
}

// SetReticulumRouter attaches a Reticulum router so routes are injected when
// bridges with Reticulum identity come online, removed on death, and refreshed
// on health reports.
func (s *Subscriber) SetReticulumRouter(r ReticulumRouter) {
	s.retRouter = r
}

// SetCertAuthority configures the CA certificate pool for verifying birth
// message signatures. If not set, birth signature verification is skipped.
func (s *Subscriber) SetCertAuthority(ca *CertAuthority) {
	if ca == nil {
		return
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.caCert)
	s.caCertPool = pool
}

// SetHeMBReassembler attaches a HeMB reassembly buffer so that bonded
// RLNC-coded symbols from bridges are decoded and the resulting payloads
// are re-published to the standard MO pipeline.
func (s *Subscriber) SetHeMBReassembler(r HeMBReassembler) {
	s.hembReassembler = r
}

// SetBirthSignatureMode configures how unsigned births are handled.
// "warn" (default): accept unsigned births, log a warning, mark bridge as unverified.
// "enforce": reject unsigned births entirely.
func (s *Subscriber) SetBirthSignatureMode(mode string) {
	if mode == BirthSignatureModeEnforce {
		s.birthSignatureMode = BirthSignatureModeEnforce
	} else {
		s.birthSignatureMode = BirthSignatureModeWarn
	}
}

// Start subscribes to all bridge lifecycle MQTT topics.
func (s *Subscriber) Start() error {
	subs := []struct {
		topic   string
		handler func(string, []byte)
	}{
		{protocol.SubBridgeBirth, s.handleBridgeBirth},
		{protocol.SubBridgeDeath, s.handleBridgeDeath},
		{protocol.SubBridgeHealth, s.handleBridgeHealth},
		{protocol.SubDeviceBirth, s.handleDeviceBirth},
		{protocol.SubDeviceDeath, s.handleDeviceDeath},
	}

	// Subscribe to HeMB bonded symbols if a reassembler is configured.
	if s.hembReassembler != nil {
		subs = append(subs, struct {
			topic   string
			handler func(string, []byte)
		}{protocol.SubBridgeHeMB, s.handleHeMBSymbol})
	}

	for _, sub := range subs {
		for _, filter := range hubmqtt.DualFilters(sub.topic) {
			if err := s.mqtt.Subscribe(filter, 1, sub.handler); err != nil {
				return fmt.Errorf("bridge subscriber: %w", err)
			}
		}
	}

	s.stopEgress = make(chan struct{})
	go s.reconcileEgressLoop(s.stopEgress)

	slog.Info("bridge: subscriber started",
		"tenant", s.tenants.Default(),
		"topics", len(subs),
		"hemb", s.hembReassembler != nil,
	)
	return nil
}

// Stop is a no-op placeholder for future unsubscribe logic.
func (s *Subscriber) Stop() {
	s.stopOnce.Do(func() {
		if s.stopEgress != nil {
			close(s.stopEgress)
		}
	})
	slog.Info("bridge: subscriber stopped")
}

// handleBridgeBirth processes a bridge birth certificate.
func (s *Subscriber) handleBridgeBirth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		slog.Debug("bridge: birth on unparseable topic", "topic", topic)
		return
	}

	var birth protocol.BridgeBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		slog.Debug("bridge: invalid birth JSON", "error", err, "bridge", bridgeID)
		return
	}

	if birth.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in birth",
			"bridge", bridgeID,
			"protocol", birth.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	// Verify birth signature if CA is configured.
	birthVerified := false
	if s.caCertPool != nil {
		err := VerifyBirthSignature(payload, birth.Signature, birth.Certificate, bridgeID, s.caCertPool)
		if err == nil {
			birthVerified = true
			slog.Info("bridge: birth signature verified",
				"bridge", bridgeID,
			)
		} else if errors.Is(err, ErrBirthUnsigned) {
			mode := s.birthSignatureMode
			if mode == "" {
				mode = BirthSignatureModeWarn
			}
			if mode == BirthSignatureModeEnforce {
				slog.Warn("bridge: rejecting unsigned birth (enforce mode)",
					"bridge", bridgeID,
				)
				return
			}
			slog.Warn("bridge: unsigned birth accepted (warn mode, upgrade bridge to enable signing)",
				"bridge", bridgeID,
			)
		} else {
			slog.Warn("bridge: birth signature verification failed, rejecting",
				"bridge", bridgeID,
				"error", err,
			)
			return
		}
	}

	// Marshal the full birth cert for storage.
	birthJSON, _ := json.Marshal(birth)

	// Build capabilities JSON array.
	capsJSON, _ := json.Marshal(birth.Capabilities)

	b := &store.Bridge{
		BridgeID:      birth.BridgeID,
		TenantID:      s.resolveTenantID(birth.TenantID, bridgeID, topic),
		Label:         birth.BridgeID,
		Hostname:      birth.Hostname,
		Version:       birth.Version,
		Mode:          birth.Mode,
		Capabilities:  string(capsJSON),
		CoTType:       birth.CoTType,
		CoTCallsign:   birth.CoTCallsign,
		Online:        true,
		LastBirth:     string(birthJSON),
		BirthVerified: birthVerified,
	}

	if birth.Location != nil {
		b.LocationLat = birth.Location.Lat
		b.LocationLon = birth.Location.Lon
		b.LocationAlt = birth.Location.Alt
	}

	if birth.Reticulum != nil {
		b.ReticulumHash = birth.Reticulum.IdentityHash
		b.ReticulumPubkey = birth.Reticulum.PublicKey
	}

	ctx := context.Background()
	tenantID := s.resolveTenantID(birth.TenantID, bridgeID, topic)

	// Detect stale retained birth messages: if the timestamp is older than
	// staleThreshold, this is a retained replay from the broker (not a fresh
	// connection). Update metadata but do NOT set online=true.
	stale := !birth.Timestamp.IsZero() && time.Since(birth.Timestamp) > s.staleThreshold

	if stale {
		// Don't overwrite online status from a retained message.
		b.Online = false
	}

	// The satellite payload formats this birth declares (HeMB-NG S3-2) and the
	// modems it names. The store writes them with the row, in one
	// transaction, ordered: the declarations by the birth's own timestamp,
	// the links by when the Hub received the birth, so a birth written again
	// later never undoes a newer one. The birth is held until it is written
	// in full, and a held birth is written again after the next birth the
	// store takes and every egressRetryEvery (egress_pending.go).
	receivedAt := time.Now().UnixNano()
	if !birth.Timestamp.IsZero() {
		b.BirthStamp = birth.Timestamp.UnixNano()
	}
	for _, iface := range birth.Interfaces {
		if iface.IMEI == "" {
			continue
		}
		if b.Links == nil {
			b.Links = map[string]int64{}
		}
		b.Links[iface.IMEI] = receivedAt
		if format := declaredFormat(iface); format != "" {
			if b.Egress == nil {
				b.Egress = map[string]store.EgressDecl{}
			}
			b.Egress[iface.IMEI] = store.EgressDecl{Format: format, Stamp: b.BirthStamp}
		}
	}

	key := s.pending().hold(tenantID, b)
	if err := s.writeHeld(ctx, key); err != nil {
		slog.Error("bridge: failed to create/update bridge", "error", err, "bridge", bridgeID)
		return
	}

	if !stale {
		if err := s.store.SetBridgeOnline(ctx, tenantID, bridgeID, true); err != nil {
			slog.Error("bridge: failed to set bridge online", "error", err, "bridge", bridgeID)
		}
	}

	// The store has just taken this birth: write what other births left
	// held now rather than at the next tick.
	s.reconcileEgress(ctx, key)

	// What this Hub understands, for the bridge to read at once (C10: sos_v2). Every replica
	// publishes the same retained value; the second copy replaces the first. A retained
	// replay is answered too: a Hub that starts (or changes HUB_SOS_V2_ENABLED) while a
	// bridge stays connected sees only that bridge's retained birth, and the bridge would
	// otherwise not learn the caps until it reconnects. Off the bus's handler goroutine: a
	// publish waits for its PUBACK, which the MQTT client cannot read while this handler holds
	// its router, and at start every retained birth arrives at once (the replica never started).
	if s.sosV2 != nil {
		on := *s.sosV2
		go func() {
			if err := sos.PublishCaps(s.mqtt, tenantID, bridgeID, on); err != nil {
				slog.Warn("bridge: hub/caps publish failed", "error", err, "bridge", bridgeID)
			}
		}()
	}

	// Inject Reticulum route for bridges that report their identity.
	// Only inject for fresh births (not stale retained replays) so we don't
	// re-create routes for bridges that have since gone offline.
	if !stale && s.retRouter != nil && birth.Reticulum != nil && birth.Reticulum.IdentityHash != "" {
		pubKey, _ := hex.DecodeString(birth.Reticulum.PublicKey)
		if s.retRouter.InjectRoute(birth.Reticulum.IdentityHash, pubKey, "mqtt", 1) {
			slog.Info("bridge: injected reticulum route",
				"bridge", bridgeID,
				"dest", birth.Reticulum.IdentityHash,
			)
		}
	}

	if stale {
		slog.Info("bridge: registered (stale retained birth, not marking online)",
			"bridge", bridgeID,
			"version", birth.Version,
			"hostname", birth.Hostname,
			"birth_age", time.Since(birth.Timestamp).Round(time.Second).String(),
		)
	} else {
		slog.Info("bridge: registered",
			"bridge", bridgeID,
			"version", birth.Version,
			"hostname", birth.Hostname,
			"interfaces", len(birth.Interfaces),
		)
	}
}

// handleBridgeDeath processes a bridge death notification.
func (s *Subscriber) handleBridgeDeath(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		slog.Debug("bridge: death on unparseable topic", "topic", topic)
		return
	}

	var death protocol.BridgeDeath
	if err := json.Unmarshal(payload, &death); err != nil {
		slog.Debug("bridge: invalid death JSON", "error", err, "bridge", bridgeID)
		return
	}

	if death.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in death",
			"bridge", bridgeID,
			"protocol", death.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	ctx := context.Background()
	tenantID := s.tenants.ForBridgeTopic(context.Background(), bridgeID, bridgeTopicTenant(topic))

	// Remove Reticulum route if the bridge had one.
	if s.retRouter != nil {
		if b, err := s.store.GetBridge(ctx, tenantID, bridgeID); err == nil && b != nil && b.ReticulumHash != "" {
			if s.retRouter.RemoveHex(b.ReticulumHash) {
				slog.Info("bridge: removed reticulum route",
					"bridge", bridgeID,
					"dest", b.ReticulumHash,
				)
			}
		}
	}

	if err := s.store.SetBridgeOnline(ctx, tenantID, bridgeID, false); err != nil {
		slog.Error("bridge: failed to set bridge offline", "error", err, "bridge", bridgeID)
		return
	}

	slog.Info("bridge: offline",
		"bridge", bridgeID,
		"reason", death.Reason,
	)
}

// handleBridgeHealth processes a periodic health report.
func (s *Subscriber) handleBridgeHealth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		return
	}

	var health protocol.BridgeHealth
	if err := json.Unmarshal(payload, &health); err != nil {
		slog.Debug("bridge: invalid health JSON", "error", err, "bridge", bridgeID)
		return
	}

	if health.Protocol != protocol.ProtocolVersion {
		return
	}

	ctx := context.Background()
	tenantID := s.tenants.ForBridgeTopic(context.Background(), bridgeID, bridgeTopicTenant(topic))

	// One statement for the whole report: health, last_seen, last report
	// (bearer, time) and online=true, because a bridge sending health is
	// unambiguously alive even if the reaper marked it offline. This was four
	// UPDATEs of the same row, and with both replicas processing every report
	// they queued behind each other's synchronous commits (MESHSAT-1155).
	if s.leader == nil || s.leader.IsLeader() {
		if err := s.store.RecordBridgeHealth(ctx, tenantID, bridgeID, string(payload), "mqtt", time.Now().UTC()); err != nil {
			slog.Debug("bridge: failed to record health", "error", err, "bridge", bridgeID)
		}
	}

	// Refresh Reticulum route TTL on each health report so routes stay alive
	// as long as the bridge is reporting healthy.
	if s.retRouter != nil {
		if b, err := s.store.GetBridge(ctx, tenantID, bridgeID); err == nil && b != nil && b.ReticulumHash != "" {
			s.retRouter.RefreshRoute(b.ReticulumHash)
		}
	}
}

// handleDeviceBirth processes a device coming online under a bridge.
func (s *Subscriber) handleDeviceBirth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	deviceID := extractDeviceIDFromTopic(topic)
	if bridgeID == "" || deviceID == "" {
		slog.Debug("bridge: device birth on unparseable topic", "topic", topic)
		return
	}

	var birth protocol.DeviceBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		slog.Debug("bridge: invalid device birth JSON", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}

	if birth.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in device birth",
			"bridge", bridgeID,
			"device", deviceID,
			"protocol", birth.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	ctx := context.Background()
	tenantID := s.tenants.ForBridgeTopic(context.Background(), bridgeID, bridgeTopicTenant(topic))

	// Auto-provision device if it has an IMEI and doesn't exist yet.
	if birth.IMEI != "" {
		existing, err := s.store.GetDevice(ctx, tenantID, birth.IMEI)
		if err != nil || existing == nil {
			// A birth for an unregistered device is a registration, so the
			// plan's ceiling applies -- but only to the row. We do not return:
			// the birth is still logged, the bridge is still online, and any
			// traffic this device sends still flows, because a subscription
			// limit must never be a reason a field kit goes unheard.
			if ok, why := s.allowAnother(ctx, tenantID); !ok {
				slog.Warn("bridge: device not auto-provisioned, tenant is at its plan's device limit",
					"bridge", bridgeID, "imei", birth.IMEI, "tenant", tenantID, "reason", why)
			} else {
				label := birth.Label
				if label == "" {
					label = birth.DeviceID
				}
				dev := &store.Device{
					IMEI:  birth.IMEI,
					Label: label,
					Type:  birth.Type,
				}
				if err := s.store.CreateDevice(ctx, tenantID, dev); err != nil {
					slog.Debug("bridge: failed to auto-provision device",
						"error", err, "bridge", bridgeID, "imei", birth.IMEI)
				} else {
					slog.Info("bridge: auto-provisioned device",
						"bridge", bridgeID,
						"imei", birth.IMEI,
						"type", birth.Type,
					)
				}
			}
		}

		if err := s.store.AssociateDeviceWithBridge(ctx, tenantID, birth.IMEI, bridgeID); err != nil {
			slog.Debug("bridge: failed to associate device",
				"error", err, "bridge", bridgeID, "imei", birth.IMEI)
		}
	}

	slog.Info("bridge: device online",
		"bridge", bridgeID,
		"device", deviceID,
		"type", birth.Type,
		"imei", birth.IMEI,
	)
}

// handleDeviceDeath processes a device going offline.
func (s *Subscriber) handleDeviceDeath(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	deviceID := extractDeviceIDFromTopic(topic)
	if bridgeID == "" || deviceID == "" {
		return
	}

	var death protocol.DeviceDeath
	if err := json.Unmarshal(payload, &death); err != nil {
		slog.Debug("bridge: invalid device death JSON", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}

	if death.Protocol != protocol.ProtocolVersion {
		return
	}

	slog.Info("bridge: device offline",
		"bridge", bridgeID,
		"device", deviceID,
		"reason", death.Reason,
	)
}

// handleHeMBSymbol processes a single HeMB RLNC-coded symbol from a bridge.
// When the reassembly buffer collects K independent symbols for a generation,
// it decodes the original payload and re-publishes it to the bridge's MO topic.
func (s *Subscriber) handleHeMBSymbol(topic string, payload []byte) {
	if s.hembReassembler == nil || len(payload) == 0 {
		return
	}

	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		slog.Debug("hemb: symbol on unparseable topic", "topic", topic)
		return
	}

	if !protocol.IsHeMBFrame(payload) {
		slog.Debug("hemb: payload is not a valid HeMB frame",
			"bridge", bridgeID, "len", len(payload))
		return
	}

	sym, streamID, bearerIdx, err := protocol.ParseHeMBSymbol(payload)
	if err != nil {
		slog.Debug("hemb: failed to parse symbol",
			"bridge", bridgeID, "error", err)
		return
	}

	metrics.HeMBSymbolsReceived.WithLabelValues(bridgeID).Inc()

	decoded, err := s.hembReassembler.AddSymbol(streamID, bearerIdx, sym)
	if err != nil {
		slog.Warn("hemb: reassembly error",
			"bridge", bridgeID,
			"stream", streamID,
			"gen", sym.GenID,
			"error", err,
		)
		return
	}

	if decoded == nil {
		return
	}

	metrics.HeMBGenerationsDecoded.WithLabelValues(bridgeID).Inc()

	// Generation fully decoded — re-publish the reassembled payload to the
	// bridge's MO decoded topic so it flows through the standard pipeline
	// (persistence, WebSocket broadcast, routing, etc.).
	// Both replicas reassemble every generation (the buffer is in-memory and
	// per-process), so without a claim the decoded payload is published twice
	// and every mo/decoded consumer sees it twice (MESHSAT-1120). The key is
	// what both replicas computed identically: the stream and generation.
	if s.store != nil {
		claimCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		won, err := s.store.ClaimOnce(claimCtx, fmt.Sprintf("hemb:%s:%d:%d", bridgeID, streamID, sym.GenID))
		cancel()
		if err != nil {
			// Fail open: a duplicated message is a nuisance, a lost one is gone.
			slog.Warn("hemb: claim failed, publishing anyway", "bridge", bridgeID, "gen", sym.GenID, "error", err)
		} else if !won {
			slog.Debug("hemb: generation already published by another replica", "bridge", bridgeID, "gen", sym.GenID)
			return
		}
	}

	moTopic := protocol.TopicDeviceMessage(bridgeID)
	if err := s.mqtt.Publish(moTopic, 1, false, decoded); err != nil {
		slog.Error("hemb: failed to publish reassembled payload",
			"bridge", bridgeID,
			"topic", moTopic,
			"error", err,
		)
		return
	}

	slog.Info("hemb: generation decoded",
		"bridge", bridgeID,
		"stream", streamID,
		"gen", sym.GenID,
		"k", sym.K,
		"payload_bytes", len(decoded),
	)
}

// resolveTenantID returns the tenant for a bridge message.
//
// A tenant named in the birth payload is a claim, not an answer. It used to be
// returned verbatim, so a bridge could put any tenant id in its own birth and
// land there — and because the upsert reassigns tenant_id on conflict, it could
// walk an already-registered bridge from one tenant into another. Anything
// built on top of tenancy, a device quota included, is worth nothing while the
// subject of the check gets to choose it.
//
// The registered owner decides. An unregistered bridge may still be placed by
// the topic it published on, which the resolver checks against real tenants;
// that is how a bridge first joins. Only the claim is refused.
func (s *Subscriber) resolveTenantID(birthTenantID, bridgeID, topic string) string {
	resolved := s.tenants.ForBridgeTopic(context.Background(), bridgeID, bridgeTopicTenant(topic))
	if birthTenantID != "" && birthTenantID != resolved {
		slog.Warn("bridge: birth claimed a tenant it does not belong to, using the owner",
			"bridge", bridgeID, "claimed", birthTenantID, "owner", resolved)
	}
	return resolved
}

// bridgeTopicTenant returns the tenant segment of a tenant-prefixed bridge
// topic, "" for the legacy shape.
func bridgeTopicTenant(topic string) string {
	tenant, _, _, ok := hubmqtt.ParseBridgeTopic(topic)
	if !ok || tenant == hubmqtt.DefaultTenant {
		return ""
	}
	return tenant
}

// extractBridgeIDFromTopic extracts bridge_id from either topic shape:
// meshsat/bridge/{id}/... or meshsat/{tenant}/bridge/{id}/...
func extractBridgeIDFromTopic(topic string) string {
	_, id, _, ok := hubmqtt.ParseBridgeTopic(topic)
	if !ok {
		return ""
	}
	return id
}

// extractDeviceIDFromTopic extracts device_id from .../bridge/{bridge_id}/device/{device_id}/...
func extractDeviceIDFromTopic(topic string) string {
	_, _, rest, ok := hubmqtt.ParseBridgeTopic(topic)
	if !ok || len(rest) < 3 || rest[0] != "device" {
		return ""
	}
	return rest[1]
}
