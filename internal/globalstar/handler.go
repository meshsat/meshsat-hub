package globalstar

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/deadman"
	"github.com/meshsat/meshsat-hub/internal/dedup"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/msvqsc"
)

// MOPayload is the JSON body POSTed by the Globalstar gateway webhook for MO messages.
type MOPayload struct {
	MessageID  string  `json:"messageId"`
	DeviceID   string  `json:"deviceId"`
	Data       string  `json:"data"` // base64-encoded payload
	ReceivedAt string  `json:"receivedAt,omitempty"`
	Latitude   float64 `json:"latitude,omitempty"`
	Longitude  float64 `json:"longitude,omitempty"`
}

// MOMessage represents a decoded Globalstar Mobile Originated message.
type MOMessage struct {
	DeviceID    string  `json:"device_id"`
	MessageID   string  `json:"message_id"`
	Channel     string  `json:"channel"`
	Text        string  `json:"text,omitempty"`
	Raw         string  `json:"raw"`
	Compressed  bool    `json:"compressed"`
	Compression string  `json:"compression,omitempty"`
	Encrypted   bool    `json:"encrypted"`
	ReceivedAt  string  `json:"received_at"`
	Latitude    float64 `json:"latitude,omitempty"`
	Longitude   float64 `json:"longitude,omitempty"`
}

// RawMOMessage is published to mo/raw for Globalstar messages.
type RawMOMessage struct {
	DeviceID   string  `json:"device_id"`
	MessageID  string  `json:"message_id"`
	Raw        string  `json:"raw"`
	Channel    string  `json:"channel"`
	ReceivedAt string  `json:"received_at"`
	Latitude   float64 `json:"latitude,omitempty"`
	Longitude  float64 `json:"longitude,omitempty"`
}

// reticulumReceiver is the interface for injecting inbound Reticulum packets.
type reticulumReceiver interface {
	OnReceive(raw []byte)
}

// Handler handles Globalstar MO webhook POST requests.
type Handler struct {
	mqtt        bus.MessageBus
	secret      string
	audit       *audit.Service
	dedup       dedup.Dedup
	reassembler *fragment.Reassembler
	keyStore    *hubcrypto.KeyStore
	deadman     *deadman.Monitor
	msvqsc      *msvqsc.Decoder
	retIface    reticulumReceiver
}

// NewHandler creates a new Globalstar webhook handler.
func NewHandler(mqtt bus.MessageBus, secret string) *Handler {
	return &Handler{mqtt: mqtt, secret: secret}
}

// SetAudit attaches an audit service for logging message_received events.
func (h *Handler) SetAudit(a *audit.Service) {
	h.audit = a
}

// SetDedup attaches a dedup tracker for duplicate MO message suppression.
func (h *Handler) SetDedup(d dedup.Dedup) {
	h.dedup = d
}

// SetReassembler attaches a fragment reassembler for MO reassembly.
func (h *Handler) SetReassembler(r *fragment.Reassembler) {
	h.reassembler = r
}

// SetKeyStore attaches a crypto keystore for E2E decryption of MO messages.
func (h *Handler) SetKeyStore(ks *hubcrypto.KeyStore) {
	h.keyStore = ks
}

// SetDeadman attaches a dead man's switch monitor for check-in on MO messages.
func (h *Handler) SetDeadman(dm *deadman.Monitor) {
	h.deadman = dm
}

// SetMSVQSC attaches an MSVQ-SC decoder for Android-compressed messages.
func (h *Handler) SetMSVQSC(d *msvqsc.Decoder) {
	h.msvqsc = d
}

// SetReticulumIface attaches a Reticulum interface for forwarding raw packets.
func (h *Handler) SetReticulumIface(iface reticulumReceiver) {
	h.retIface = iface
}

func (h *Handler) publish(topic string, qos byte, retained bool, v any) {
	if h.mqtt == nil {
		return
	}
	if err := h.mqtt.PublishJSON(topic, qos, retained, v); err != nil {
		slog.Error("globalstar: mqtt publish failed", "error", err, "topic", topic)
	}
}

// ServeHTTP handles the webhook POST from the Globalstar gateway.
//
//	@Summary      Receive Globalstar MO message
//	@Description  Globalstar gateway posts MO messages here via webhook callback
//	@Tags         webhook
//	@Accept       json
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Router       /api/webhook/globalstar [post]
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 1MB.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Verify HMAC signature — reject unsigned requests when secret is not configured.
	if h.secret == "" {
		slog.Warn("globalstar: webhook secret not configured, rejecting unsigned request")
		http.Error(w, `{"error":"webhook secret not configured"}`, http.StatusForbidden)
		return
	}
	if !h.verifySignature(r) {
		slog.Warn("globalstar: signature verification failed")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var payload MOPayload
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		slog.Warn("globalstar: decode payload failed", "error", err)
		http.Error(w, `{"error":"invalid json payload"}`, http.StatusBadRequest)
		return
	}

	if payload.DeviceID == "" {
		http.Error(w, `{"error":"missing deviceId"}`, http.StatusBadRequest)
		return
	}

	// Base64-decode the data payload.
	rawBytes, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		slog.Warn("globalstar: base64 decode failed", "error", err, "device", payload.DeviceID)
		http.Error(w, `{"error":"invalid base64 data"}`, http.StatusBadRequest)
		return
	}

	// Dedup: check if this message has already been processed.
	if h.dedup != nil {
		dedupHash := sha256.Sum256(rawBytes)
		dedupKey := fmt.Sprintf("gs:%s:%s:%x", payload.DeviceID, payload.MessageID, dedupHash[:8])
		if !h.dedup.IsNew(dedupKey) {
			slog.Info("globalstar: duplicate MO suppressed", "device", payload.DeviceID, "msg", payload.MessageID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
			return
		}
	}

	// Forward to Reticulum relay if this could be a Reticulum packet.
	if h.retIface != nil && len(rawBytes) >= 19 {
		h.retIface.OnReceive(rawBytes)
	}

	rawB64 := base64.StdEncoding.EncodeToString(rawBytes)
	receivedAt := payload.ReceivedAt
	if receivedAt == "" {
		receivedAt = time.Now().UTC().Format(time.RFC3339)
	}

	slog.Info("globalstar: MO received",
		"device", payload.DeviceID,
		"message", payload.MessageID,
		"bytes", len(rawBytes),
		"received_at", receivedAt,
	)

	deviceID := payload.DeviceID

	// Publish raw payload to mo/raw.
	rawMsg := RawMOMessage{
		DeviceID:   payload.DeviceID,
		MessageID:  payload.MessageID,
		Raw:        rawB64,
		Channel:    "globalstar",
		ReceivedAt: receivedAt,
		Latitude:   payload.Latitude,
		Longitude:  payload.Longitude,
	}
	h.publish(hubmqtt.TopicMORaw(deviceID), 1, false, rawMsg)

	// Fragment reassembly: Globalstar uses the same 2-byte Iridium fragment header format.
	// [fragment_index:4bit | total_fragments:4bit] [message_id:8bit]
	// 128-byte MTU, fragment payload = 126 bytes.
	if h.reassembler != nil && fragment.IsFragment(rawBytes) {
		reassembled, fragErr := h.reassembler.AddFragment(deviceID, rawBytes)
		if fragErr != nil {
			slog.Warn("globalstar: fragment error", "error", fragErr, "device", deviceID, "msg", payload.MessageID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_error"})
			return
		}
		if reassembled == nil {
			slog.Info("globalstar: fragment buffered, waiting for more",
				"device", deviceID, "msg", payload.MessageID,
				"pending", h.reassembler.PendingCount(),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_buffered"})
			return
		}
		slog.Info("globalstar: message reassembled from fragments",
			"device", deviceID, "bytes", len(reassembled),
		)
		rawBytes = reassembled
		rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
	}

	// Strip protocol version byte (if present) before further processing.
	protoVersion, strippedBytes := codec.StripVersionByte(rawBytes)
	if protoVersion > 0 {
		slog.Info("globalstar: protocol version detected",
			"device", deviceID, "version", protoVersion)
		rawBytes = strippedBytes
	} else {
		slog.Debug("globalstar: legacy message (no version byte)", "device", deviceID)
	}
	if protoVersion > 0 && protoVersion != codec.ProtoVersion1 {
		slog.Warn("globalstar: protocol version mismatch, processing anyway",
			"device", deviceID, "version", protoVersion, "expected", codec.ProtoVersion1)
	}

	// Attempt E2E decryption if payload is large enough to be GCM-encrypted.
	encrypted := false
	if h.keyStore != nil && len(rawBytes) >= hubcrypto.Overhead {
		decrypted, err := h.keyStore.DecryptMessage(deviceID, rawBytes)
		if err == nil {
			slog.Info("globalstar: message decrypted",
				"device", deviceID, "encrypted_bytes", len(rawBytes), "decrypted_bytes", len(decrypted))
			rawBytes = decrypted
			rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
			encrypted = true
		}
	}

	// Attempt SMAZ2 decompression.
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
		if decErr == nil {
			text = decoded
			compressed = true
			compressionType = "msvqsc"
		}
	} else {
		if isPrintable(rawBytes) {
			text = string(rawBytes)
		}
	}

	// Publish decoded message to mo/decoded.
	decoded := MOMessage{
		DeviceID:    payload.DeviceID,
		MessageID:   payload.MessageID,
		Channel:     "globalstar",
		Text:        text,
		Raw:         rawB64,
		Compressed:  compressed,
		Compression: compressionType,
		Encrypted:   encrypted,
		ReceivedAt:  receivedAt,
		Latitude:    payload.Latitude,
		Longitude:   payload.Longitude,
	}
	h.publish(hubmqtt.TopicMODecoded(deviceID), 1, false, decoded)

	// Publish position if lat/lon are present and non-zero.
	if payload.Latitude != 0 || payload.Longitude != 0 {
		pos := positionMessage{
			Lat:       payload.Latitude,
			Lon:       payload.Longitude,
			Source:    "globalstar",
			Timestamp: receivedAt,
		}
		h.publish(hubmqtt.TopicPosition(deviceID), 1, true, pos)
	}

	// Dead man's switch: device sent an MO message, reset its timer.
	if h.deadman != nil {
		h.deadman.CheckIn(deviceID)
	}

	// Audit: log message_received event (use RemoteAddr directly — never trust X-Forwarded-For in webhook handlers).
	if h.audit != nil {
		tid := auth.TenantIDFromContext(r.Context())
		detail := fmt.Sprintf("device=%s message=%s bytes=%d channel=globalstar", payload.DeviceID, payload.MessageID, len(rawBytes))
		ip := r.RemoteAddr
		if err := h.audit.Log(r.Context(), tid, "message_received", "webhook", detail, ip); err != nil {
			slog.Warn("audit: failed to log message_received", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// verifySignature checks the HMAC-SHA256 signature from Globalstar webhook headers.
func (h *Handler) verifySignature(r *http.Request) bool {
	sig := r.Header.Get("X-Signature")
	if sig == "" {
		return false
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		slog.Warn("globalstar: read body for signature check failed", "error", err)
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(bodyBytes)
	expected := fmt.Sprintf("%x", mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}

	// Replace body so JSON decoder can read it.
	r.Body = readCloser(bodyBytes)
	return true
}

// positionMessage is published to the position topic.
type positionMessage struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Source    string  `json:"source"`
	Timestamp string  `json:"timestamp"`
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
