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

// IntegrationHandler returns the status of all inbound integration channels.
type IntegrationHandler struct {
	cfg config.Config
}

// NewIntegrationHandler creates a new IntegrationHandler from the Hub config.
func NewIntegrationHandler(cfg config.Config) *IntegrationHandler {
	return &IntegrationHandler{cfg: cfg}
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
	enabled := h.cfg.TAKEnabled && h.cfg.TAKHost != ""
	cfg := map[string]string{}

	if h.cfg.TAKHost != "" {
		cfg["host"] = h.cfg.TAKHost
	} else {
		cfg["host"] = "not set"
	}
	if h.cfg.TAKPort > 0 {
		cfg["port"] = strconv.Itoa(h.cfg.TAKPort)
	} else {
		cfg["port"] = "8087 (default)"
	}
	if h.cfg.TAKSSL {
		cfg["ssl"] = "enabled"
	} else {
		cfg["ssl"] = "disabled"
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

	return IntegrationStatus{
		Name:        "TAK (CoT Gateway)",
		Type:        "tcp",
		Enabled:     enabled,
		Connected:   enabled,
		Description: "OpenTAK Server CoT XML integration — forwards device positions, SOS, telemetry, and chat as Cursor on Target events via TCP",
		Config:      cfg,
		Setup:       "Set HUB_TAK_ENABLED=true and HUB_TAK_HOST to your TAK server address. Optional: HUB_TAK_PORT (default 8087), HUB_TAK_SSL, HUB_TAK_CALLSIGN_PREFIX, HUB_TAK_COT_STALE_SECONDS.",
	}
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
