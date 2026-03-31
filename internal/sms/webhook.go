package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/compress"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/deadman"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/msvqsc"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// InboundSMS is published to MQTT when an SMS is received via webhook.
type InboundSMS struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Body        string `json:"body"`
	MessageSID  string `json:"message_sid,omitempty"`
	Channel     string `json:"channel"`
	Compressed  bool   `json:"compressed"`
	Compression string `json:"compression,omitempty"`
	Encrypted   bool   `json:"encrypted"`
	Timestamp   string `json:"timestamp"`
}

// RawSMS is published to mo/raw for binary SMS payloads.
type RawSMS struct {
	From      string `json:"from"`
	Raw       string `json:"raw"` // base64
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
}

// reticulumReceiver is the interface for injecting inbound Reticulum packets.
type reticulumReceiver interface {
	OnReceive(raw []byte)
}

// WebhookHandler handles inbound SMS webhooks from Twilio/Vonage.
// [MESHSAT-446] Now matches Rock7/Cloudloop pipeline: dedup, fragment
// reassembly, SMAZ2/MSVQ-SC compression, Reticulum relay, bridge uplink.
type WebhookHandler struct {
	mqtt        bus.MessageBus
	secret      string // webhook validation secret
	store       store.Store
	keyStore    *hubcrypto.KeyStore
	dedup       dedup.Dedup
	reassembler *fragment.Reassembler
	msvqsc      *msvqsc.Decoder
	retIface    reticulumReceiver
	deadman     *deadman.Monitor
	audit       *audit.Service
}

// NewWebhookHandler creates a new inbound SMS webhook handler.
func NewWebhookHandler(mqtt bus.MessageBus, secret string) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt, secret: secret}
}

// SetStore enables message persistence for inbound SMS.
func (h *WebhookHandler) SetStore(s store.Store) { h.store = s }

// SetKeyStore enables E2E decryption of inbound SMS messages.
func (h *WebhookHandler) SetKeyStore(ks *hubcrypto.KeyStore) { h.keyStore = ks }

// SetDedup attaches a dedup tracker for duplicate SMS suppression. [MESHSAT-446]
func (h *WebhookHandler) SetDedup(d dedup.Dedup) { h.dedup = d }

// SetReassembler attaches a fragment reassembler for multi-SMS payloads. [MESHSAT-446]
func (h *WebhookHandler) SetReassembler(r *fragment.Reassembler) { h.reassembler = r }

// SetMSVQSC attaches an MSVQ-SC decoder for Android-compressed messages. [MESHSAT-446]
func (h *WebhookHandler) SetMSVQSC(d *msvqsc.Decoder) { h.msvqsc = d }

// SetReticulumIface attaches a Reticulum interface for forwarding raw packets. [MESHSAT-446]
func (h *WebhookHandler) SetReticulumIface(iface reticulumReceiver) { h.retIface = iface }

// SetDeadman attaches a dead man's switch monitor. [MESHSAT-446]
func (h *WebhookHandler) SetDeadman(dm *deadman.Monitor) { h.deadman = dm }

// SetAudit attaches an audit service for logging. [MESHSAT-446]
func (h *WebhookHandler) SetAudit(a *audit.Service) { h.audit = a }

func (h *WebhookHandler) publish(topic string, qos byte, retained bool, v any) {
	if h.mqtt == nil {
		return
	}
	if err := h.mqtt.PublishJSON(topic, qos, retained, v); err != nil {
		slog.Error("sms: mqtt publish failed", "error", err, "topic", topic)
	}
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

	// SMS payloads can be plaintext or base64-encoded binary.
	// Try base64 decode first — if it succeeds, treat as binary pipeline.
	// If it fails, treat as plaintext SMS (legacy path).
	rawBytes, b64Err := base64.StdEncoding.DecodeString(body)
	isBinary := b64Err == nil && len(rawBytes) > 0

	if isBinary {
		h.processBinaryPipeline(r, w, from, to, messageSID, rawBytes)
	} else {
		h.processPlaintextSMS(r, w, from, to, body, messageSID)
	}
}

// processBinaryPipeline handles base64-encoded binary SMS payloads through the
// full pipeline: dedup → bridge uplink → Reticulum → fragment → decrypt →
// decompress → persist. Matches Rock7/Cloudloop processing. [MESHSAT-446]
func (h *WebhookHandler) processBinaryPipeline(r *http.Request, w http.ResponseWriter,
	from, to, messageSID string, rawBytes []byte) {

	// Dedup: check if this message has already been processed.
	if h.dedup != nil {
		dedupHash := sha256.Sum256(rawBytes)
		dedupKey := fmt.Sprintf("sms:%s:%x", from, dedupHash[:8])
		if !h.dedup.IsNew(dedupKey) {
			slog.Info("sms: duplicate suppressed", "from", from, "sid", messageSID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
			return
		}
	}

	// Bridge satellite uplink detection (magic 0x4D53 "MS").
	if h.handleBridgeUplink(r.Context(), from, rawBytes) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "bridge_uplink"})
		return
	}

	// Reticulum packet relay — forward raw packets to Reticulum interface.
	if h.retIface != nil && len(rawBytes) >= 19 {
		h.retIface.OnReceive(rawBytes)
	}

	rawB64 := base64.StdEncoding.EncodeToString(rawBytes)

	// Publish raw payload to mo/raw.
	h.publish(hubmqtt.TopicMORaw(from), 1, false, RawSMS{
		From:      from,
		Raw:       rawB64,
		Channel:   "sms",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	// Fragment reassembly: if payload is a fragment, collect and reassemble.
	if h.reassembler != nil && fragment.IsFragment(rawBytes) {
		reassembled, fragErr := h.reassembler.AddFragment(from, rawBytes)
		if fragErr != nil {
			slog.Warn("sms: fragment error", "error", fragErr, "from", from)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_error"})
			return
		}
		if reassembled == nil {
			slog.Info("sms: fragment buffered, waiting for more",
				"from", from, "pending", h.reassembler.PendingCount())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_buffered"})
			return
		}
		slog.Info("sms: message reassembled from fragments",
			"from", from, "bytes", len(reassembled))
		rawBytes = reassembled
		rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
	}

	// E2E decryption.
	encrypted := false
	if h.keyStore != nil && len(rawBytes) >= hubcrypto.Overhead {
		if decrypted, err := h.keyStore.DecryptMessage(from, rawBytes); err == nil {
			rawBytes = decrypted
			rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
			encrypted = true
			slog.Info("sms: decrypted", "from", from, "bytes", len(decrypted))
		} else if decrypted, err := h.keyStore.DecryptMessage("sms", rawBytes); err == nil {
			rawBytes = decrypted
			rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
			encrypted = true
		}
	}

	// Decompression: SMAZ2 → MSVQ-SC → raw UTF-8 fallback.
	text := ""
	compressed := false
	compressionType := ""

	decompressed, err := compress.Decompress(rawBytes)
	if err == nil && len(decompressed) > 0 && utf8.Valid(decompressed) {
		text = string(decompressed)
		compressed = true
		compressionType = "smaz2"
	} else if h.msvqsc != nil && msvqsc.LooksLikeMSVQSC(rawBytes) {
		decoded, decErr := h.msvqsc.Decode(rawBytes)
		if decErr == nil && decoded != "" {
			text = decoded
			compressed = true
			compressionType = "msvqsc"
		}
	} else if utf8.Valid(rawBytes) {
		text = string(rawBytes)
	}

	// Publish decoded message.
	msg := InboundSMS{
		From:        from,
		To:          to,
		Body:        text,
		MessageSID:  messageSID,
		Channel:     "sms",
		Compressed:  compressed,
		Compression: compressionType,
		Encrypted:   encrypted,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	h.publish(hubmqtt.TopicMODecoded(from), 1, false, msg)
	h.publish("meshsat/hub/sms/inbound", 1, false, msg)

	// Persist to database.
	if h.store != nil {
		tid := auth.TenantIDFromContext(r.Context())
		status := "received"
		if encrypted {
			status = "decrypted"
		}
		dbMsg := &store.Message{
			ID:         fmt.Sprintf("sms-in-%d", time.Now().UnixNano()),
			DeviceIMEI: from,
			Direction:  "mo",
			Channel:    "sms",
			Text:       text,
			RawHex:     rawB64,
			Compressed: compressed,
			Status:     status,
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.store.InsertMessage(ctx, tid, dbMsg); err != nil {
			slog.Warn("sms: persist failed", "error", err)
		}
	}

	// Dead man's switch: sender checked in.
	if h.deadman != nil {
		h.deadman.CheckIn(from)
	}

	// Audit log.
	if h.audit != nil {
		tid := auth.TenantIDFromContext(r.Context())
		detail := fmt.Sprintf("from=%s bytes=%d compressed=%v encrypted=%v", from, len(rawBytes), compressed, encrypted)
		_ = h.audit.Log(r.Context(), tid, "message_received", "webhook_sms", detail, r.RemoteAddr)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// processPlaintextSMS handles plain-text SMS (not base64-encoded). This is the
// legacy path for human-readable SMS messages that aren't part of the binary
// MeshSat pipeline.
func (h *WebhookHandler) processPlaintextSMS(r *http.Request, w http.ResponseWriter,
	from, to, body, messageSID string) {

	msg := InboundSMS{
		From:       from,
		To:         to,
		Body:       body,
		MessageSID: messageSID,
		Channel:    "sms",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	h.publish("meshsat/hub/sms/inbound", 1, false, msg)

	// Persist inbound SMS.
	if h.store != nil {
		tid := auth.TenantIDFromContext(r.Context())
		dbMsg := &store.Message{
			ID:         fmt.Sprintf("sms-in-%d", time.Now().UnixNano()),
			DeviceIMEI: from,
			Direction:  "mo",
			Channel:    "sms",
			Text:       body,
			Status:     "received",
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.store.InsertMessage(ctx, tid, dbMsg); err != nil {
			slog.Warn("sms: persist failed", "error", err)
		}
	}

	// Dead man's switch.
	if h.deadman != nil {
		h.deadman.CheckIn(from)
	}

	// Twilio expects a TwiML XML response. Empty <Response/> = don't reply.
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><Response/>"))
}

// handleBridgeUplink checks if the raw bytes are a bridge uplink message
// (magic 0x4D53) and processes it. Returns true if handled. [MESHSAT-446]
func (h *WebhookHandler) handleBridgeUplink(ctx context.Context, from string, rawBytes []byte) bool {
	if !bridge.IsBridgeSatUplink(rawBytes) {
		return false
	}

	msgType, payload, err := bridge.DecodeSatUplink(rawBytes)
	if err != nil {
		slog.Warn("sms: bridge uplink decode failed", "error", err, "from", from)
		return false
	}

	switch msgType {
	case bridge.SatMsgPosition:
		bridgeID, lat, lon, alt, _, ts, err := bridge.DecodeSatPosition(payload)
		if err != nil {
			slog.Warn("sms: bridge position decode failed", "error", err, "from", from)
			return true
		}
		slog.Info("sms: bridge uplink position",
			"bridge_id", bridgeID, "lat", lat, "lon", lon, "alt", alt, "from", from)
		h.publish(hubmqtt.TopicPosition(from), 1, true, map[string]any{
			"lat": lat, "lon": lon, "alt": alt,
			"source": "sms_uplink", "timestamp": ts.Format(time.RFC3339),
		})

	case bridge.SatMsgSOS:
		bridgeID, deviceID, lat, lon, message, ts, err := bridge.DecodeSatSOS(payload)
		if err != nil {
			slog.Warn("sms: bridge SOS decode failed", "error", err, "from", from)
			return true
		}
		slog.Warn("sms: BRIDGE SOS via SMS",
			"bridge_id", bridgeID, "device_id", deviceID,
			"lat", lat, "lon", lon, "message", message, "from", from)
		h.publish(hubmqtt.TopicSOS(bridgeID), 1, false, map[string]any{
			"bridge_id": bridgeID, "device_id": deviceID,
			"lat": lat, "lon": lon, "message": message,
			"source": "sms", "timestamp": ts.Format(time.RFC3339),
		})

	case bridge.SatMsgHealthSummary:
		slog.Info("sms: bridge health uplink via SMS", "from", from, "bytes", len(payload))

	default:
		slog.Info("sms: unknown bridge uplink type via SMS",
			"from", from, "type", msgType, "bytes", len(payload))
	}

	// Mark bridge as online.
	if h.store != nil {
		tid := auth.TenantIDFromContext(context.Background())
		_ = h.store.SetBridgeOnline(ctx, tid, from, true)
	}

	return true
}
