package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/store"
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
	mqtt     bus.MessageBus
	secret   string // webhook validation secret
	store    store.Store
	keyStore *hubcrypto.KeyStore
}

// NewWebhookHandler creates a new inbound SMS webhook handler.
func NewWebhookHandler(mqtt bus.MessageBus, secret string) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt, secret: secret}
}

// SetStore enables message persistence for inbound SMS.
func (h *WebhookHandler) SetStore(s store.Store) { h.store = s }

// SetKeyStore enables E2E decryption of inbound SMS messages.
func (h *WebhookHandler) SetKeyStore(ks *hubcrypto.KeyStore) { h.keyStore = ks }

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

	// Try to decrypt if the body looks like base64-encoded ciphertext.
	decryptedBody := body
	encrypted := false
	if h.keyStore != nil {
		if raw, err := base64.StdEncoding.DecodeString(body); err == nil && len(raw) >= hubcrypto.Overhead {
			// Try sender-specific key first, then global "sms" key.
			if pt, err := h.keyStore.DecryptMessage(from, raw); err == nil {
				decryptedBody = string(pt)
				encrypted = true
				slog.Info("sms: decrypted with sender key", "from", from, "plaintext_len", len(pt))
			} else if pt, err := h.keyStore.DecryptMessage("sms", raw); err == nil {
				decryptedBody = string(pt)
				encrypted = true
				slog.Info("sms: decrypted with global sms key", "from", from, "plaintext_len", len(pt))
			} else {
				slog.Warn("sms: decryption failed, storing as-is", "from", from, "error", err)
			}
		}
	}

	msg := InboundSMS{
		From:       from,
		To:         to,
		Body:       decryptedBody,
		MessageSID: messageSID,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	if h.mqtt != nil {
		if err := h.mqtt.PublishJSON("meshsat/hub/sms/inbound", 1, false, msg); err != nil {
			slog.Error("sms: mqtt publish failed", "error", err)
		}
	}

	// Persist inbound SMS to messages table.
	if h.store != nil {
		status := "received"
		if encrypted {
			status = "decrypted"
		}
		dbMsg := &store.Message{
			ID:         fmt.Sprintf("sms-in-%d", time.Now().UnixNano()),
			DeviceIMEI: from,
			Direction:  "mo",
			Channel:    "sms",
			Text:       decryptedBody,
			Status:     status,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.store.InsertMessage(ctx, store.DefaultTenantID, dbMsg); err != nil {
			slog.Warn("sms: persist failed", "error", err)
		}
	}

	// Twilio expects a TwiML XML response. Empty <Response/> = don't reply.
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><Response/>"))
}
