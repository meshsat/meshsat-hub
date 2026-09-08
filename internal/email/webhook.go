package email

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// InboundEmail is published to MQTT when an email is received.
type InboundEmail struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	PGPSigned bool   `json:"pgp_signed"`
	PGPSigner string `json:"pgp_signer,omitempty"`
	Decrypted bool   `json:"decrypted"`
	Timestamp string `json:"timestamp"`
}

// WebhookHandler handles inbound email webhooks from services like
// Mailgun, SendGrid, or custom SMTP-to-webhook bridges.
type WebhookHandler struct {
	mqtt    bus.MessageBus
	keyRing *KeyRing
	secret  string // shared secret (X-Webhook-Secret or ?secret=); empty = every request refused
}

// tenantOf is the tenant whose webhook path this mail arrived at, or the
// platform tenant on the legacy shared path. Inbound mail used to be published
// on one global topic for every tenant at once, so anything subscribed to the
// hub namespace saw all of it (MESHSAT-975).
func (h *WebhookHandler) tenantOf(r *http.Request) string {
	if t := webhookroute.TenantID(r.Context()); t != "" {
		return t
	}
	return hubmqtt.DefaultTenant
}

// SetSecret configures the shared secret the email service must present (MESHSAT-976).
func (h *WebhookHandler) SetSecret(s string) { h.secret = s }

func (h *WebhookHandler) secretOK(r *http.Request) bool {
	// A request that came through a tenant's webhook path already proved it
	// holds that tenant's secret; the middleware resolved it.
	if webhookroute.TenantID(r.Context()) != "" {
		return true
	}
	got := r.Header.Get("X-Webhook-Secret")
	if got == "" {
		got = r.URL.Query().Get("secret")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) == 1
}

// NewWebhookHandler creates a new inbound email webhook handler.
func NewWebhookHandler(mqtt bus.MessageBus, kr *KeyRing) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt, keyRing: kr}
}

// ServeHTTP handles the inbound email webhook POST.
//
//	@Summary      Receive inbound email
//	@Description  Email service (Mailgun/SendGrid) posts inbound emails here
//	@Tags         webhook
//	@Accept       application/x-www-form-urlencoded
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Failure      403  {object}  map[string]string
//	@Router       /api/webhook/email [post]
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if h.secret == "" {
		slog.Warn("email: webhook secret not configured, rejecting request")
		http.Error(w, `{"error":"webhook secret not configured"}`, http.StatusForbidden)
		return
	}
	if !h.secretOK(r) {
		slog.Warn("email: webhook secret missing or wrong", "remote", r.RemoteAddr)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB limit for email content

	if err := r.ParseForm(); err != nil {
		slog.Warn("email: parse form failed", "error", err)
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	from := r.FormValue("from")
	if from == "" {
		from = r.FormValue("sender")
	}
	to := r.FormValue("to")
	if to == "" {
		to = r.FormValue("recipient")
	}
	subject := r.FormValue("subject")
	body := r.FormValue("body-plain")
	if body == "" {
		body = r.FormValue("body")
	}
	if body == "" {
		body = r.FormValue("text")
	}

	if from == "" || body == "" {
		http.Error(w, `{"error":"missing from or body"}`, http.StatusBadRequest)
		return
	}

	slog.Info("email: inbound received", "from", from, "to", to, "subject", subject, "len", len(body))

	msg := InboundEmail{
		From:      from,
		To:        to,
		Subject:   subject,
		Body:      body,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Try PGP decryption if body looks PGP-encrypted.
	if h.keyRing != nil && strings.Contains(body, "-----BEGIN PGP MESSAGE-----") {
		plaintext, signer, err := h.keyRing.Decrypt(body)
		if err != nil {
			slog.Warn("email: PGP decryption failed", "from", from, "error", err)
		} else {
			msg.Body = plaintext
			msg.Decrypted = true
			if signer != "" {
				msg.PGPSigned = true
				msg.PGPSigner = signer
			}
			slog.Info("email: PGP decrypted", "from", from, "signer", signer)
		}
	}

	if h.mqtt != nil {
		topic := hubmqtt.Namespace(h.tenantOf(r)) + "/hub/email/inbound"
		if err := h.mqtt.PublishJSON(topic, 1, false, msg); err != nil {
			slog.Error("email: mqtt publish failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
