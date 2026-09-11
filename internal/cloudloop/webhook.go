package cloudloop

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/deadman"
	"github.com/meshsat/meshsat-hub/internal/dedup"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/msvqsc"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// reticulumReceiver is the interface for injecting inbound Reticulum packets.
type reticulumReceiver interface {
	OnReceive(raw []byte)
}

// WebhookMOMessage represents a decoded Cloudloop MO message published to MQTT.
type WebhookMOMessage struct {
	ID               string  `json:"id"`
	IMEI             string  `json:"imei"`
	MOMSN            int     `json:"momsn"`
	Channel          string  `json:"channel"`
	Text             string  `json:"text,omitempty"`
	RawHex           string  `json:"raw_hex"`
	Compressed       bool    `json:"compressed"`
	Compression      string  `json:"compression,omitempty"`
	Encrypted        bool    `json:"encrypted"`
	TransmitTime     string  `json:"transmit_time"`
	IridiumLatitude  float64 `json:"iridium_latitude,omitempty"`
	IridiumLongitude float64 `json:"iridium_longitude,omitempty"`
	IridiumCEP       float64 `json:"iridium_cep,omitempty"`
	Source           string  `json:"source"`
}

// WebhookRawMessage is the raw MO payload published to mo/raw.
type WebhookRawMessage struct {
	IMEI             string  `json:"imei"`
	MOMSN            int     `json:"momsn"`
	Raw              string  `json:"raw"`
	Channel          string  `json:"channel"`
	TransmitTime     string  `json:"transmit_time"`
	IridiumLatitude  float64 `json:"iridium_latitude,omitempty"`
	IridiumLongitude float64 `json:"iridium_longitude,omitempty"`
	IridiumCEP       float64 `json:"iridium_cep,omitempty"`
	Source           string  `json:"source"`
}

// WebhookPositionMessage is published when location data is present.
type WebhookPositionMessage struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	CEP       float64 `json:"cep,omitempty"`
	Source    string  `json:"source"`
	Timestamp string  `json:"timestamp"`
}

// WebhookHandler handles Cloudloop LingoMO webhook POST requests.
type WebhookHandler struct {
	tenants         *tenancy.Resolver // device → tenant for topic namespaces; nil = default tenant
	mqtt            bus.MessageBus
	audit           *audit.Service
	dedup           dedup.Dedup
	reassembler     *fragment.Reassembler
	keyStore        *hubcrypto.KeyStore
	deadman         *deadman.Monitor
	msvqsc          *msvqsc.Decoder
	retIface        reticulumReceiver
	hembReassembler interface{ AddRawFrame([]byte) ([]byte, error) }
	resolver        *ThingResolver
	allowedIPs      []string
	// expected is observe-only: it never refuses a request. See source.go.
	expected expectedSources
	token    string                // platform webhook token (default tenant); required when allowedIPs is "*" and no accounts
	accounts *integrations.Service // per-tenant webhook tokens (MESHSAT-977)
	oob      OOBClassifier         // management frames (MESHSAT-964)
	store    interface {
		InsertMessage(ctx context.Context, tenantID string, m *store.Message) error
		SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
		SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
		TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error
		SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error
	}
}

// NewWebhookHandler creates a new Cloudloop webhook handler.
func NewWebhookHandler(mqtt bus.MessageBus) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt}
}

// SetAudit attaches an audit service for logging message_received events.
func (h *WebhookHandler) SetAudit(a *audit.Service) {
	h.audit = a
}

// SetDedup attaches a dedup tracker for duplicate MO message suppression.
func (h *WebhookHandler) SetDedup(d dedup.Dedup) {
	h.dedup = d
}

// SetReassembler attaches a fragment reassembler for MO reassembly.
func (h *WebhookHandler) SetReassembler(r *fragment.Reassembler) {
	h.reassembler = r
}

// SetKeyStore attaches a crypto keystore for E2E decryption of MO messages.
func (h *WebhookHandler) SetKeyStore(ks *hubcrypto.KeyStore) {
	h.keyStore = ks
}

// SetDeadman attaches a dead man's switch monitor for check-in on MO messages.
func (h *WebhookHandler) SetDeadman(dm *deadman.Monitor) {
	h.deadman = dm
}

// SetMSVQSC attaches an MSVQ-SC decoder for Android-compressed messages.
func (h *WebhookHandler) SetMSVQSC(d *msvqsc.Decoder) {
	h.msvqsc = d
}

// SetStore attaches a store for persisting MO messages and bridge state.
func (h *WebhookHandler) SetStore(s interface {
	InsertMessage(ctx context.Context, tenantID string, m *store.Message) error
	SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
	SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
	TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error
	SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error
}) {
	h.store = s
}

// uplinkSink builds the shared bridge uplink sink (MESHSAT-964 B).
func (h *WebhookHandler) uplinkSink() *bridge.UplinkSink {
	var st bridge.UplinkStore
	if h.store != nil {
		st = h.store
	}
	return bridge.NewUplinkSink(st, h.publish, h.audit, "cloudloop_webhook").SetTenants(h.tenants)
}

// bearerOf maps a LingoMO source to the bearer name used in fleet state.
func bearerOf(source string) string {
	switch source {
	case "cloudloop_imt":
		return "imt"
	case "cloudloop_cellular":
		return "cellular"
	default:
		return "sbd"
	}
}

// SetReticulumIface attaches a Reticulum interface for forwarding raw packets.
func (h *WebhookHandler) SetReticulumIface(iface reticulumReceiver) {
	h.retIface = iface
}

// SetHeMBReassembler attaches a HeMB reassembly buffer for inbound coded symbols.
func (h *WebhookHandler) SetHeMBReassembler(r interface{ AddRawFrame([]byte) ([]byte, error) }) {
	h.hembReassembler = r
}

// SetAllowedIPs sets the IP allowlist for webhook requests.
// If empty, all IPs are allowed.

// OOBClassifier is the out-of-band management service: it takes "MS:" frames
// out of the message flow before anything else sees them (MESHSAT-964 C).
type OOBClassifier interface {
	HandleInbound(ctx context.Context, bearer, origin, text string) bool
}

// SetOOB attaches the out-of-band classifier.
func (h *WebhookHandler) SetOOB(c OOBClassifier) { h.oob = c }

// SetToken sets the shared webhook token. With a wildcard allowlist the token
// is the only thing that authenticates Cloudloop, so ServeHTTP refuses every
// request until one is configured (MESHSAT-971).
func (h *WebhookHandler) SetToken(t string) { h.token = t }

// SetAccounts enables per-tenant webhook tokens: the token in the request
// selects the tenant, and the device must belong to it.
func (h *WebhookHandler) SetAccounts(a *integrations.Service) { h.accounts = a }

// presentedToken returns the token from the query or header ("" if none).
func presentedToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return r.Header.Get("X-Webhook-Token")
}

// statusWrongTenant is returned by processLingoMO when the authenticated
// tenant does not own the device; ServeHTTP turns it into 403.
const statusWrongTenant = "device belongs to another tenant"

// wildcardAllowlist reports whether the allowlist is the catch-all "*".
func (h *WebhookHandler) wildcardAllowlist() bool {
	return len(h.allowedIPs) == 1 && strings.TrimSpace(h.allowedIPs[0]) == "*"
}

// tokenOK compares the presented token (query or header) in constant time.
func (h *WebhookHandler) tokenOK(r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if got == "" {
		got = r.Header.Get("X-Webhook-Token")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func (h *WebhookHandler) SetAllowedIPs(ips []string) {
	h.allowedIPs = ips
}

// SetExpectedSources configures the observe-only source range. It does NOT gate
// anything -- a delivery from outside it is still processed -- and that is the
// point: this is the path an SOS arrives on.
func (h *WebhookHandler) SetExpectedSources(ips []string) {
	h.expected = parseExpectedSources(ips)
}

// SetResolver attaches a ThingResolver for learning IMEI-to-thingID mappings
// from incoming MO messages. Each processed LingoMO teaches the resolver
// about the device's thingId and modem type (SBD vs IMT).
func (h *WebhookHandler) SetResolver(r *ThingResolver) {
	h.resolver = r
}

func (h *WebhookHandler) publish(topic string, qos byte, retained bool, v any) {
	if h.mqtt == nil {
		return
	}
	if err := h.mqtt.PublishJSON(topic, qos, retained, v); err != nil {
		slog.Error("cloudloop: mqtt publish failed", "error", err, "topic", topic)
	}
}

// ServeHTTP handles the webhook POST from Cloudloop Ground Control.
//
//	@Summary      Receive Cloudloop LingoMO message
//	@Description  Cloudloop posts LingoMO JSON messages here (SBD, IMT, or cellular)
//	@Tags         webhook
//	@Accept       json
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      403  {object}  map[string]string
//	@Router       /api/webhook/cloudloop [post]
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Observation first, and unconditionally: it must run for every delivery,
	// including ones a later check refuses, or the record of where traffic comes
	// from would have a hole exactly where it matters.
	h.noteSource(r, "cloudloop")

	// IP allowlist check — reject all requests when no allowlist is configured.
	// Use "*" to allow all IPs (useful when webhook signature validation suffices).
	if len(h.allowedIPs) == 0 {
		slog.Warn("cloudloop: webhook IP allowlist not configured, rejecting request")
		http.Error(w, `{"error":"webhook IP allowlist not configured"}`, http.StatusForbidden)
		return
	}
	// The tenant comes from the secret in the request path (MESHSAT-975),
	// resolved before this handler runs and independent of how the IP
	// allowlist is configured. Previously the token was only consulted under a
	// wildcard allowlist, so a concrete allowlist silently ignored it.
	tokenTenant := webhookroute.TenantID(r.Context())
	if tokenTenant != "" {
		// Already authenticated by the secret in the path. A concrete IP
		// allowlist still applies as the second control; a wildcard one has
		// nothing left to say, because the secret is what the wildcard was
		// waiting for.
		if !h.wildcardAllowlist() && !h.isAllowedIP(r) {
			slog.Warn("cloudloop: request from disallowed IP", "remote", r.RemoteAddr)
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
	} else if h.wildcardAllowlist() {
		// A wildcard allowlist is only acceptable together with a token: the
		// platform token (default tenant) or a tenant's own webhook token.
		if h.token == "" && h.accounts == nil {
			slog.Warn("cloudloop: allowlist is * but no webhook token configured, rejecting request")
			http.Error(w, `{"error":"webhook token not configured"}`, http.StatusForbidden)
			return
		}
		tok := presentedToken(r)
		if tok != "" && h.accounts != nil {
			tenant, _, err := h.accounts.LookupByToken(r.Context(), integrations.ProviderCloudloop, "webhook_token", tok)
			if err != nil {
				slog.Error("cloudloop: webhook token lookup failed", "error", err)
			}
			tokenTenant = tenant
		}
		if tokenTenant == "" && h.token != "" && h.tokenOK(r) {
			tokenTenant = store.DefaultTenantID
		}
		if tokenTenant == "" {
			slog.Warn("cloudloop: webhook token missing or wrong", "remote", r.RemoteAddr)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	} else if !h.isAllowedIP(r) {
		slog.Warn("cloudloop: request from disallowed IP", "remote", r.RemoteAddr)
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}

	// Limit request body to 1MB (IMT payloads are max 100KB).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var mo LingoMO
	if err := json.NewDecoder(r.Body).Decode(&mo); err != nil {
		slog.Warn("cloudloop: JSON decode failed", "error", err)
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if tokenTenant != "" {
		ctx = tenancy.WithTenant(ctx, tokenTenant)
	}
	status := h.processLingoMO(ctx, &mo, r.RemoteAddr)
	if status == statusWrongTenant {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// ProcessLingoMO processes a LingoMO message from any source (webhook or MQTT subscriber).
func (h *WebhookHandler) ProcessLingoMO(ctx context.Context, mo *LingoMO) string {
	return h.processLingoMO(ctx, mo, "mqtt")
}

func (h *WebhookHandler) processLingoMO(ctx context.Context, mo *LingoMO, remoteAddr string) string {
	imei := mo.ExtractIMEI()
	if imei == "" {
		// Log the full LingoMO for debugging (test messages may lack IMEI)
		slog.Warn("cloudloop: no IMEI in LingoMO", "id", mo.ID,
			"has_identity_hw", mo.Identity.Hardware != nil,
			"has_sbd", mo.SBD != nil,
			"has_imt", mo.IMT != nil,
			"has_cellular", mo.Cellular != nil,
			"thing_id", mo.Identity.ThingID,
			"account_id", mo.Identity.AccountID)
		return "error_no_imei"
	}
	// A webhook token authenticates one tenant: the device must be that
	// tenant's (or unregistered, in which case it joins it). A device owned by
	// another tenant is refused so one tenant cannot inject into another.
	if want := tenancy.FromContext(ctx); want != "" && h.tenants != nil {
		if owner := h.tenants.ForDeviceTopic(ctx, imei, want); owner != want {
			slog.Warn("cloudloop: webhook token tenant does not own the device", "imei", imei, "token_tenant", want, "owner", owner)
			return statusWrongTenant
		}
	}

	// Teach the resolver about this device's thingId and modem type.
	if h.resolver != nil {
		h.resolver.LearnFromMO(h.tenantOf(ctx, imei), mo)
	}

	momsn := mo.MOMSN()
	transmitTime := mo.TransmitTime().Format(time.RFC3339)
	source := mo.Source()
	lat, lon, cep, hasLocation := mo.Location()

	// Decode base64 payload.
	rawBytes, err := base64.StdEncoding.DecodeString(mo.Message)
	if err != nil {
		slog.Warn("cloudloop: base64 decode failed", "error", err, "imei", imei)
		return "error_decode"
	}

	// Dedup by LingoMO.ID (UUID from Cloudloop).
	if h.dedup != nil {
		dedupHash := sha256.Sum256(rawBytes)
		dedupKey := fmt.Sprintf("cl:%s:%x", mo.ID, dedupHash[:8])
		if !h.dedup.IsNew(dedupKey) {
			slog.Info("cloudloop: duplicate MO suppressed", "imei", imei, "id", mo.ID)
			return "duplicate"
		}
	}

	// HeMB frame detection — intercept before other processing.
	if protocol.IsHeMBFrame(rawBytes) {
		slog.Info("cloudloop: HeMB frame detected", "imei", imei, "bytes", len(rawBytes))
		if h.hembReassembler != nil {
			if decoded, err := h.hembReassembler.AddRawFrame(rawBytes); err != nil {
				slog.Warn("cloudloop: HeMB reassembly error", "error", err, "imei", imei)
			} else if decoded != nil {
				slog.Info("cloudloop: HeMB decoded", "imei", imei, "payload_bytes", len(decoded))
			}
		}
		return "hemb_symbol"
	}

	// Check if this is a bridge satellite uplink message (magic 0x4D53 "MS").
	if h.handleBridgeSatUplink(ctx, imei, rawBytes, bearerOf(source)) {
		return "bridge_sat_uplink"
	}

	// Check if this is a Reticulum packet and forward to the relay.
	if h.retIface != nil && len(rawBytes) >= 19 {
		h.retIface.OnReceive(rawBytes)
	}

	rawB64 := base64.StdEncoding.EncodeToString(rawBytes)

	slog.Info("cloudloop: MO received",
		"imei", imei,
		"id", mo.ID,
		"source", source,
		"bytes", len(rawBytes),
		"transmit_time", transmitTime,
	)

	// Publish raw payload to mo/raw.
	rawMsg := WebhookRawMessage{
		IMEI:             imei,
		MOMSN:            momsn,
		Raw:              rawB64,
		Channel:          "iridium",
		TransmitTime:     transmitTime,
		IridiumLatitude:  lat,
		IridiumLongitude: lon,
		IridiumCEP:       cep,
		Source:           source,
	}
	h.publish(hubmqtt.TopicMORawFor(h.tenantOf(ctx, imei), imei), 1, false, rawMsg)

	// Fragment reassembly.
	if h.reassembler != nil && fragment.IsFragment(rawBytes) {
		reassembled, fragErr := h.reassembler.AddFragment(imei, rawBytes)
		if fragErr != nil {
			slog.Warn("cloudloop: fragment error", "error", fragErr, "imei", imei, "id", mo.ID)
			return "fragment_error"
		}
		if reassembled == nil {
			_, _, msgID := fragment.DecodeHeader(rawBytes[0], rawBytes[1])
			slog.Info("cloudloop: fragment buffered, waiting for more",
				"imei", imei, "id", mo.ID, "msg_id", msgID,
				"pending", h.reassembler.PendingCount(),
			)
			return "fragment_buffered"
		}
		slog.Info("cloudloop: message reassembled from fragments",
			"imei", imei, "bytes", len(reassembled),
		)
		rawBytes = reassembled
		rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
	}

	// Strip protocol version byte (if present).
	protoVersion, strippedBytes := codec.StripVersionByte(rawBytes)
	if protoVersion > 0 {
		slog.Info("cloudloop: protocol version detected",
			"imei", imei, "version", protoVersion)
		rawBytes = strippedBytes
	}
	if protoVersion > 0 && protoVersion != codec.ProtoVersion1 {
		slog.Warn("cloudloop: protocol version mismatch, processing anyway",
			"imei", imei, "version", protoVersion, "expected", codec.ProtoVersion1)
	}

	// Attempt E2E decryption.
	encrypted := false
	if h.keyStore != nil && len(rawBytes) >= hubcrypto.Overhead {
		decrypted, err := h.keyStore.DecryptMessage(imei, rawBytes)
		if err == nil {
			slog.Info("cloudloop: message decrypted",
				"imei", imei, "encrypted_bytes", len(rawBytes), "decrypted_bytes", len(decrypted))
			rawBytes = decrypted
			rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
			encrypted = true
		}
	}

	// Attempt decompression.
	text := ""
	compressed := false
	compressionType := ""

	decompressed, err := compress.Decompress(rawBytes)
	if err == nil && len(decompressed) > 0 && isPrintable(decompressed) {
		text = string(decompressed)
		compressed = true
		compressionType = "smaz2"
	} else if h.msvqsc != nil && msvqsc.LooksLikeMSVQSC(rawBytes) {
		decoded, decErr := h.msvqsc.Decode(rawBytes)
		if decErr == nil && decoded != "" {
			text = decoded
			compressed = true
			compressionType = "msvqsc"
			slog.Info("cloudloop: MSVQ-SC decoded", "imei", imei, "text", text)
		}
	} else {
		if isPrintable(rawBytes) {
			text = string(rawBytes)
		}
	}

	// An OOB management frame is a reply to a command the Hub sent; it never
	// enters the message pipeline.
	if h.oob != nil && text != "" && h.oob.HandleInbound(ctx, bearerOf(source), imei, text) {
		return "oob_frame"
	}

	// Publish decoded message to mo/decoded.
	msgID := fmt.Sprintf("mo-%s-%d", imei, momsn) // same scheme as rockblock.sbdMessageID
	decoded := WebhookMOMessage{
		ID:               msgID,
		IMEI:             imei,
		MOMSN:            momsn,
		Channel:          "iridium",
		Text:             text,
		RawHex:           rawB64,
		Compressed:       compressed,
		Compression:      compressionType,
		Encrypted:        encrypted,
		TransmitTime:     transmitTime,
		IridiumLatitude:  lat,
		IridiumLongitude: lon,
		IridiumCEP:       cep,
		Source:           source,
	}
	h.publish(hubmqtt.TopicMODecodedFor(h.tenantOf(ctx, imei), imei), 1, false, decoded)

	// Persist MO message to database.
	if h.store != nil {
		tid := h.tenantOf(ctx, imei)
		msg := &store.Message{
			ID:         msgID,
			DeviceIMEI: imei,
			Direction:  "mo",
			Channel:    "iridium",
			MOMSN:      momsn,
			Text:       text,
			RawHex:     rawB64,
			Compressed: compressed,
			Status:     "received",
			Lat:        lat,
			Lon:        lon,
		}
		if err := h.store.InsertMessage(ctx, tid, msg); errors.Is(err, store.ErrDuplicate) {
			slog.Debug("cloudloop: message already persisted", "id", msgID)
		} else if err != nil {
			slog.Warn("cloudloop: message persist failed", "error", err, "imei", imei)
		} else {
			slog.Info("cloudloop: message persisted", "imei", imei, "id", mo.ID)
		}
	}

	// Publish position if lat/lon are present.
	if hasLocation && (lat != 0 || lon != 0) {
		pos := WebhookPositionMessage{
			Lat:       lat,
			Lon:       lon,
			CEP:       cep,
			Source:    source,
			Timestamp: transmitTime,
		}
		h.publish(hubmqtt.TopicPositionFor(h.tenantOf(ctx, imei), imei), 1, true, pos)
	}

	// Dead man's switch: device sent an MO message, reset its timer.
	if h.deadman != nil {
		h.deadman.CheckIn(imei)
	}

	// Audit: log message_received event.
	if h.audit != nil {
		tid := h.tenantOf(ctx, imei)
		detail := fmt.Sprintf("imei=%s id=%s source=%s bytes=%d", imei, mo.ID, source, len(rawBytes))
		ip := remoteAddr
		if idx := strings.LastIndex(ip, ":"); idx > 0 {
			ip = ip[:idx]
		}
		if err := h.audit.Log(ctx, tid, "message_received", "cloudloop_webhook", detail, ip); err != nil {
			slog.Warn("audit: failed to log message_received", "error", err)
		}
	}

	return "ok"
}

// handleBridgeSatUplink hands a bridge uplink frame (magic 0x4D53) to the
// shared sink. Returns true if the bytes were a frame.
func (h *WebhookHandler) handleBridgeSatUplink(ctx context.Context, imei string, rawBytes []byte, bearer string) bool {
	if !bridge.IsBridgeSatUplink(rawBytes) {
		return false
	}
	return h.uplinkSink().Handle(ctx, h.tenantOf(ctx, imei), bearer, imei, rawBytes)
}

// trustedProxyNets defines private/Docker subnets from which X-Forwarded-For is trusted.
var trustedProxyNets = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",     // RFC 1918
		"172.16.0.0/12",  // RFC 1918 / Docker bridge
		"192.168.0.0/16", // RFC 1918
		"127.0.0.0/8",    // loopback
		"::1/128",        // IPv6 loopback
		"fd00::/8",       // IPv6 ULA
	}
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

// isTrustedProxy checks if an IP belongs to a known reverse proxy subnet.
func isTrustedProxy(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range trustedProxyNets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// isAllowedIP checks if the request IP is in the allowlist.
// X-Forwarded-For is only trusted when the direct connection is from a known proxy subnet.
func (h *WebhookHandler) isAllowedIP(r *http.Request) bool {
	directIP := r.RemoteAddr
	// Strip port from RemoteAddr.
	if host, _, err := net.SplitHostPort(directIP); err == nil {
		directIP = host
	}

	ip := directIP
	// Only trust X-Forwarded-For if direct connection is from a trusted proxy.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" && isTrustedProxy(directIP) {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}

	for _, allowed := range h.allowedIPs {
		if ip == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}

// isPrintable checks if bytes are valid printable UTF-8 text.
func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return len(b) > 0
}

// SetTenants makes published topics follow the owning tenant's namespace
// (MESHSAT-864 MR 20). Without it every topic uses the default namespace.
func (h *WebhookHandler) SetTenants(r *tenancy.Resolver) { h.tenants = r }

// tenantOf is the tenant every row and topic for this message uses. On a
// tenant's own webhook path it is that tenant, already checked to own the
// device. On the shared platform account it is the device's registered owner,
// and the platform itself for a device nobody has registered. What it must
// never be is a default chosen because a lookup missed (MESHSAT-975).
func (h *WebhookHandler) tenantOf(ctx context.Context, id string) string {
	auth := tenancy.FromContext(ctx)
	if auth == "" {
		auth = hubmqtt.DefaultTenant
	}
	if h.tenants == nil {
		return auth
	}
	return h.tenants.ForDeviceTopic(ctx, id, auth)
}
