package email

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
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
//	@Router       /api/webhook/email [post]
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
		if err := h.mqtt.PublishJSON("meshsat/hub/email/inbound", 1, false, msg); err != nil {
			slog.Error("email: mqtt publish failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
