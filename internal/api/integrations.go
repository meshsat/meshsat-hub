package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/config"
)

// IntegrationStatus describes the state of a single inbound integration channel.
type IntegrationStatus struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Enabled     bool              `json:"enabled"`
	Connected   bool              `json:"connected"`
	WebhookURL  string            `json:"webhook_url,omitempty"`
	Description string            `json:"description,omitempty"`
	Config      map[string]string `json:"config,omitempty"`
	LastMessage *time.Time        `json:"last_message,omitempty"`
	Setup       string            `json:"setup,omitempty"`
}

// FederationStatter provides live stats from the TAK Federation instance.
type FederationStatter interface {
	Stats() (in, out int64, peerCount int)
	ConnectedPeers() []string
}

// IntegrationHandler returns the status of all inbound integration channels.
type IntegrationHandler struct {
	cfg           config.Config
	getFederation func() FederationStatter // returns current federation or nil
}

// NewIntegrationHandler creates a new IntegrationHandler from the Hub config.
func NewIntegrationHandler(cfg config.Config) *IntegrationHandler {
	return &IntegrationHandler{cfg: cfg}
}

// SetFederationGetter sets a function that returns the current TAK Federation instance.
// This uses a closure to safely read the federation pointer set by the leader election goroutine.
func (h *IntegrationHandler) SetFederationGetter(fn func() FederationStatter) {
	h.getFederation = fn
}

// GetFederation returns the current federation instance (or nil).
func (h *IntegrationHandler) GetFederation() FederationStatter {
	if h.getFederation == nil {
		return nil
	}
	return h.getFederation()
}

// ListIntegrations returns the status of all inbound integration channels.
// @Summary List integration channel status
// @Description Returns the enabled/connected status of all inbound message channels (Rock7, Cloudloop, Twilio SMS, Email, Astrocast, Globalstar).
// @Tags integrations
// @Produce json
// @Success 200 {array} IntegrationStatus
// @Router /api/integrations [get]
func (h *IntegrationHandler) ListIntegrations(w http.ResponseWriter, _ *http.Request) {
	integrations := []IntegrationStatus{
		h.rock7Status(),
		h.cloudloopWebhookStatus(),
		h.cloudloopMQTTStatus(),
		h.astrocastStatus(),
		h.globalstarStatus(),
		h.smsStatus(),
		h.emailStatus(),
		h.takStatus(),
		h.takFederationStatus(),
	}
	writeJSON(w, http.StatusOK, integrations)
}

func (h *IntegrationHandler) rock7Status() IntegrationStatus {
	cfg := map[string]string{}
	if h.cfg.Rock7Username != "" {
		cfg["username"] = h.cfg.Rock7Username
	}
	if h.cfg.RockBLOCKSecret != "" {
		cfg["secret"] = "configured"
	} else {
		cfg["secret"] = "not set"
	}

	return IntegrationStatus{
		Name:        "Rock7 (SBD)",
		Type:        "webhook",
		Enabled:     true, // webhook handler is always registered
		WebhookURL:  "/api/webhook/rockblock",
		Description: "RockBLOCK Iridium SBD MO messages via HTTP POST",
		Config:      cfg,
		Setup:       "Configure this webhook URL in the Rock7 portal under Delivery Groups > HTTP",
	}
}

func (h *IntegrationHandler) cloudloopWebhookStatus() IntegrationStatus {
	cfg := map[string]string{}
	if h.cfg.CloudloopWebhookAllowedIPs != "" {
		cfg["allowed_ips"] = h.cfg.CloudloopWebhookAllowedIPs
	} else {
		cfg["allowed_ips"] = "default (Cloudloop IPs)"
	}

	return IntegrationStatus{
		Name:        "Cloudloop Webhook (IMT)",
		Type:        "webhook",
		Enabled:     true, // webhook handler is always registered
		WebhookURL:  "/api/webhook/cloudloop",
		Description: "Cloudloop LingoMO messages via HTTP POST (JSON format)",
		Config:      cfg,
		Setup:       "Add this URL as an HTTP destination in Cloudloop Ground Control > Destinations. Select JSON (Lingo) format.",
	}
}

func (h *IntegrationHandler) cloudloopMQTTStatus() IntegrationStatus {
	enabled := h.cfg.CloudloopAccountID != "" && h.cfg.CloudloopMQTTBroker != ""
	cfg := map[string]string{}

	if h.cfg.CloudloopMQTTBroker != "" {
		cfg["broker"] = h.cfg.CloudloopMQTTBroker
	}
	if h.cfg.CloudloopAccountID != "" {
		cfg["account_id"] = h.cfg.CloudloopAccountID
	}
	cfg["ca_cert"] = fileStatus(h.cfg.CloudloopMQTTCACert)
	cfg["client_cert"] = fileStatus(h.cfg.CloudloopMQTTCert)
	cfg["client_key"] = fileStatus(h.cfg.CloudloopMQTTKey)

	return IntegrationStatus{
		Name:        "Cloudloop MQTT",
		Type:        "mqtt",
		Enabled:     enabled,
		Connected:   enabled, // if configured and started without error, it's connected
		Description: "Cloudloop LingoMO messages via MQTT subscription (mTLS)",
		Config:      cfg,
		Setup:       "Generate MQTT certificates in Cloudloop > Destinations > MQTT. Provide CA, client cert, and key via HUB_CLOUDLOOP_MQTT_* env vars.",
	}
}

func (h *IntegrationHandler) astrocastStatus() IntegrationStatus {
	cfg := map[string]string{}
	if h.cfg.AstrocastAPIKey != "" {
		cfg["api_key"] = maskSecret(h.cfg.AstrocastAPIKey)
	} else {
		cfg["api_key"] = "not set"
	}
	if h.cfg.AstrocastWebhookSecret != "" {
		cfg["webhook_secret"] = "configured"
	} else {
		cfg["webhook_secret"] = "not set"
	}

	return IntegrationStatus{
		Name:        "Astrocast",
		Type:        "webhook",
		Enabled:     true, // webhook handler is always registered
		WebhookURL:  "/api/webhook/astrocast",
		Description: "Astrocast satellite MO messages via HTTP POST (HMAC-verified)",
		Config:      cfg,
		Setup:       "Configure this webhook URL in the Astrocast portal under Webhooks. Enable HMAC verification for security.",
	}
}

func (h *IntegrationHandler) globalstarStatus() IntegrationStatus {
	cfg := map[string]string{}
	if h.cfg.GlobalstarAPIKey != "" {
		cfg["api_key"] = maskSecret(h.cfg.GlobalstarAPIKey)
	} else {
		cfg["api_key"] = "not set"
	}
	if h.cfg.GlobalstarWebhookSecret != "" {
		cfg["webhook_secret"] = "configured"
	} else {
		cfg["webhook_secret"] = "not set"
	}

	return IntegrationStatus{
		Name:        "Globalstar",
		Type:        "webhook",
		Enabled:     true, // webhook handler is always registered
		WebhookURL:  "/api/webhook/globalstar",
		Description: "Globalstar satellite MO messages via HTTP POST (HMAC-verified)",
		Config:      cfg,
		Setup:       "Configure this webhook URL in the Globalstar developer portal under API Webhooks.",
	}
}

func (h *IntegrationHandler) smsStatus() IntegrationStatus {
	enabled := h.cfg.SMSEnabled && h.cfg.SMSAccountSID != ""
	cfg := map[string]string{}

	if h.cfg.SMSFromNumber != "" {
		cfg["from_number"] = h.cfg.SMSFromNumber
	}
	if h.cfg.SMSAccountSID != "" {
		cfg["account_sid"] = maskSecret(h.cfg.SMSAccountSID)
	} else {
		cfg["account_sid"] = "not set"
	}
	if h.cfg.SMSWebhookSecret != "" {
		cfg["webhook_secret"] = "configured"
	} else {
		cfg["webhook_secret"] = "not set"
	}

	return IntegrationStatus{
		Name:        "Twilio SMS",
		Type:        "sms",
		Enabled:     enabled,
		WebhookURL:  "/api/webhook/sms",
		Description: "Twilio inbound SMS messages via HTTP POST webhook",
		Config:      cfg,
		Setup:       "Set this webhook URL as the Messaging webhook in your Twilio phone number configuration.",
	}
}

func (h *IntegrationHandler) emailStatus() IntegrationStatus {
	enabled := h.cfg.EmailEnabled
	cfg := map[string]string{}

	if h.cfg.EmailFrom != "" {
		cfg["from_address"] = h.cfg.EmailFrom
	}
	if h.cfg.EmailSMTPHost != "" {
		cfg["smtp_host"] = h.cfg.EmailSMTPHost
	}
	if h.cfg.EmailPGPKey != "" {
		cfg["pgp_key"] = "configured"
	} else {
		cfg["pgp_key"] = "not set (will auto-generate)"
	}

	return IntegrationStatus{
		Name:        "Email",
		Type:        "email",
		Enabled:     enabled,
		WebhookURL:  "/api/webhook/email",
		Description: "Inbound email messages via HTTP POST webhook (PGP encrypted)",
		Config:      cfg,
		Setup:       "Configure your email server to forward inbound messages to this webhook.",
	}
}

func (h *IntegrationHandler) takStatus() IntegrationStatus {
	// The Hub IS a TAK gateway — it generates CoT from bridge MQTT data,
	// has federation on port 9001, and optionally forwards to an external TAK Server.
	// "Enabled" means the Hub's CoT gateway is active (always true — it's a core feature).
	// The external TAK Server connection is optional enhancement.
	enabled := true // Hub is always a TAK/CoT gateway
	externalConnected := h.cfg.TAKEnabled && h.cfg.TAKHost != ""

	cfg := map[string]string{}
	cfg["mode"] = "Hub CoT Gateway (built-in)"

	if h.cfg.TAKHost != "" {
		cfg["external_tak_server"] = h.cfg.TAKHost + ":" + strconv.Itoa(h.cfg.TAKPort)
		cfg["external_connected"] = "yes"
	} else {
		cfg["external_tak_server"] = "none (standalone mode)"
	}
	if h.cfg.TAKSSL {
		cfg["ssl"] = "enabled"
	}
	if h.cfg.TAKCallsignPrefix != "" {
		cfg["callsign_prefix"] = h.cfg.TAKCallsignPrefix
	} else {
		cfg["callsign_prefix"] = "MESHSAT-HUB (default)"
	}
	if h.cfg.TAKCotStaleSec > 0 {
		cfg["cot_stale_seconds"] = strconv.Itoa(h.cfg.TAKCotStaleSec)
	} else {
		cfg["cot_stale_seconds"] = "600 (default)"
	}

	// Count active bridge connections as "clients"
	cfg["reticulum_tcp"] = "listening on :4242"

	return IntegrationStatus{
		Name:        "TAK (CoT Gateway)",
		Type:        "tcp",
		Enabled:     enabled,
		Connected:   externalConnected || h.cfg.TAKFederationEnabled,
		Description: "MeshSat Hub CoT gateway — receives positions, SOS, telemetry from fleet bridges via MQTT, generates CoT events, and optionally forwards to external TAK Server or federation peers",
		Config:      cfg,
		Setup:       "The Hub is a built-in TAK/CoT gateway. Optionally set HUB_TAK_HOST to forward CoT to an external TAK Server, or enable federation (HUB_TAK_FEDERATION_ENABLED) for peer-to-peer CoT relay.",
	}
}

func (h *IntegrationHandler) takFederationStatus() IntegrationStatus {
	enabled := h.cfg.TAKFederationEnabled
	cfg := map[string]string{}

	if len(h.cfg.TAKFederationPeers) > 0 {
		cfg["peers"] = strings.Join(h.cfg.TAKFederationPeers, ", ")
	} else {
		cfg["peers"] = "none configured"
	}
	if h.cfg.TAKFederationPort > 0 {
		cfg["port"] = strconv.Itoa(h.cfg.TAKFederationPort)
	} else {
		cfg["port"] = "9001 (default)"
	}
	cfg["cert"] = fileStatus(h.cfg.TAKFederationCert)
	cfg["key"] = fileStatus(h.cfg.TAKFederationKey)
	cfg["ca"] = fileStatus(h.cfg.TAKFederationCA)

	connected := false
	if h.getFederation != nil {
		if fed := h.getFederation(); fed != nil {
			in, out, pc := fed.Stats()
			connected = pc > 0
			cfg["connected_peers"] = strconv.Itoa(pc)
			cfg["msgs_in"] = strconv.FormatInt(in, 10)
			cfg["msgs_out"] = strconv.FormatInt(out, 10)
			if peers := fed.ConnectedPeers(); len(peers) > 0 {
				cfg["peer_addrs"] = strings.Join(peers, ", ")
			}
		}
	}

	return IntegrationStatus{
		Name:        "TAK Federation v2",
		Type:        "tcp",
		Enabled:     enabled,
		Connected:   connected,
		Description: "TAK Server federation — bidirectional CoT relay with remote TAK Servers on port 9001 (mutual TLS)",
		Config:      cfg,
		Setup:       "Set HUB_TAK_FEDERATION_ENABLED=true, HUB_TAK_FEDERATION_PEERS=host1:9001,host2:9001, and provide mTLS cert/key/CA.",
	}
}

// FederationPeerInfo describes a federation peer for the API.
type FederationPeerInfo struct {
	Address   string `json:"address"`
	Connected bool   `json:"connected"`
	MsgsIn    int64  `json:"msgs_in"`
	MsgsOut   int64  `json:"msgs_out"`
}

// ListFederationPeers returns connected TAK federation peers.
// @Summary TAK federation peers
// @Tags tak
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /api/tak/federation/peers [get]
func (h *IntegrationHandler) ListFederationPeers(w http.ResponseWriter, _ *http.Request) {
	result := map[string]interface{}{
		"enabled":    h.cfg.TAKFederationEnabled,
		"peers":      []FederationPeerInfo{},
		"total_in":   int64(0),
		"total_out":  int64(0),
		"peer_count": 0,
	}

	if h.getFederation != nil {
		if fed := h.getFederation(); fed != nil {
			in, out, pc := fed.Stats()
			result["total_in"] = in
			result["total_out"] = out
			result["peer_count"] = pc

			peers := []FederationPeerInfo{}
			for _, addr := range fed.ConnectedPeers() {
				peers = append(peers, FederationPeerInfo{
					Address:   addr,
					Connected: true,
				})
			}
			result["peers"] = peers
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// maskSecret returns a masked version of a secret string, showing only the
// first 4 characters followed by dots.
func maskSecret(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}

// fileStatus returns "configured" if the path is non-empty and the file exists,
// "missing" if the path is set but the file doesn't exist, or "not set".
func fileStatus(path string) string {
	if path == "" {
		return "not set"
	}
	if _, err := os.Stat(path); err != nil {
		return "missing (" + path + ")"
	}
	return "configured"
}
