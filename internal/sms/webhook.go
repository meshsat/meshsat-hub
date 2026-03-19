package sms

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
)

// InboundSMS is published to MQTT when an SMS is received via webhook.
type InboundSMS struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Body       string `json:"body"`
	MessageSID string `json:"message_sid,omitempty"`
	Timestamp  string `json:"timestamp"`
}

// WebhookHandler handles inbound SMS webhooks from Twilio/Vonage.
type WebhookHandler struct {
	mqtt   bus.MessageBus
	secret string // webhook validation secret
}

// NewWebhookHandler creates a new inbound SMS webhook handler.
func NewWebhookHandler(mqtt bus.MessageBus, secret string) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt, secret: secret}
}

// ServeHTTP handles the inbound SMS webhook POST.
//
//	@Summary      Receive inbound SMS
//	@Description  Twilio/Vonage posts inbound SMS messages here
//	@Tags         webhook
//	@Accept       application/x-www-form-urlencoded
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Router       /api/webhook/sms [post]
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if err := r.ParseForm(); err != nil {
		slog.Warn("sms: parse form failed", "error", err)
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	// Verify signature if secret is configured.
	if h.secret != "" {
		sig := r.Header.Get("X-Twilio-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Signature")
		}
		if sig == "" {
			slog.Warn("sms: missing signature header")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mac := hmac.New(sha256.New, []byte(h.secret))
		mac.Write([]byte(r.FormValue("From") + r.FormValue("Body")))
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			slog.Warn("sms: signature verification failed")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	}

	from := r.FormValue("From")
	to := r.FormValue("To")
	body := r.FormValue("Body")
	messageSID := r.FormValue("MessageSid")

	if from == "" || body == "" {
		http.Error(w, `{"error":"missing From or Body"}`, http.StatusBadRequest)
		return
	}

	slog.Info("sms: inbound received", "from", from, "to", to, "sid", messageSID, "len", len(body))

	msg := InboundSMS{
		From:       from,
		To:         to,
		Body:       body,
		MessageSID: messageSID,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	if h.mqtt != nil {
		if err := h.mqtt.PublishJSON("meshsat/hub/sms/inbound", 1, false, msg); err != nil {
			slog.Error("sms: mqtt publish failed", "error", err)
		}
	}

	// Twilio expects a TwiML response or 200 OK.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
