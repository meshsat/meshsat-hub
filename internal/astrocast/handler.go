package astrocast

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/compress"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/deadman"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
)

// MOPayload is the JSON body POSTed by the Astrocast portal webhook for MO messages.
// Reference: https://docs.astrocast.com/docs/api/overview
type MOPayload struct {
	MessageGUID  string  `json:"messageGuid"`
	DeviceGUID   string  `json:"deviceGuid"`
	Data         string  `json:"data"` // base64-encoded payload
	TransmitTime string  `json:"receivedDate,omitempty"`
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
	CallbackURL  string  `json:"callbackUrl,omitempty"`
}

// MOMessage represents a decoded Astrocast Mobile Originated message.
type MOMessage struct {
	DeviceGUID   string  `json:"device_guid"`
	MessageGUID  string  `json:"message_guid"`
	Channel      string  `json:"channel"`
	Text         string  `json:"text,omitempty"`
	Raw          string  `json:"raw"`
	Compressed   bool    `json:"compressed"`
	Compression  string  `json:"compression,omitempty"`
	Encrypted    bool    `json:"encrypted"`
	TransmitTime string  `json:"transmit_time"`
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
}

// RawMOMessage is published to mo/raw for Astrocast messages.
type RawMOMessage struct {
	DeviceGUID   string  `json:"device_guid"`
	MessageGUID  string  `json:"message_guid"`
	Raw          string  `json:"raw"`
	Channel      string  `json:"channel"`
	TransmitTime string  `json:"transmit_time"`
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
}

// Handler handles Astrocast MO webhook POST requests.
type Handler struct {
	mqtt        bus.MessageBus
	secret      string
	audit       *audit.Service
	dedup       dedup.Dedup
	reassembler *fragment.Reassembler
	keyStore    *hubcrypto.KeyStore
	deadman     *deadman.Monitor
}

// NewHandler creates a new Astrocast webhook handler.
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

func (h *Handler) publish(topic string, qos byte, retained bool, v any) {
	if h.mqtt == nil {
		return
	}
	if err := h.mqtt.PublishJSON(topic, qos, retained, v); err != nil {
		slog.Error("astrocast: mqtt publish failed", "error", err, "topic", topic)
	}
}

// ServeHTTP handles the webhook POST from the Astrocast portal.
//
//	@Summary      Receive Astrocast MO message
//	@Description  Astrocast portal posts MO messages here via webhook callback
//	@Tags         webhook
//	@Accept       json
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Router       /api/webhook/astrocast [post]
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 1MB.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Verify HMAC signature if secret is configured.
	if h.secret != "" {
		if !h.verifySignature(r) {
			slog.Warn("astrocast: signature verification failed")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	}

	var payload MOPayload
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		slog.Warn("astrocast: decode payload failed", "error", err)
		http.Error(w, `{"error":"invalid json payload"}`, http.StatusBadRequest)
		return
	}

	if payload.DeviceGUID == "" {
		http.Error(w, `{"error":"missing deviceGuid"}`, http.StatusBadRequest)
		return
	}

	// Base64-decode the data payload.
	rawBytes, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		slog.Warn("astrocast: base64 decode failed", "error", err, "device", payload.DeviceGUID)
		http.Error(w, `{"error":"invalid base64 data"}`, http.StatusBadRequest)
		return
	}

	// Dedup: check if this message has already been processed.
	// Key: deviceGUID:messageGUID:sha256(data).
	if h.dedup != nil {
		dedupHash := sha256.Sum256(rawBytes)
		dedupKey := fmt.Sprintf("ac:%s:%s:%x", payload.DeviceGUID, payload.MessageGUID, dedupHash[:8])
		if !h.dedup.IsNew(dedupKey) {
			slog.Info("astrocast: duplicate MO suppressed", "device", payload.DeviceGUID, "msg", payload.MessageGUID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
			return
		}
	}

	rawB64 := base64.StdEncoding.EncodeToString(rawBytes)
	transmitTime := payload.TransmitTime
	if transmitTime == "" {
		transmitTime = time.Now().UTC().Format(time.RFC3339)
	}

	slog.Info("astrocast: MO received",
		"device", payload.DeviceGUID,
		"message", payload.MessageGUID,
		"bytes", len(rawBytes),
		"transmit_time", transmitTime,
	)

	// Use device GUID as device identifier for MQTT topics.
	deviceID := payload.DeviceGUID

	// Publish raw payload to mo/raw.
	rawMsg := RawMOMessage{
		DeviceGUID:   payload.DeviceGUID,
		MessageGUID:  payload.MessageGUID,
		Raw:          rawB64,
		Channel:      "astrocast",
		TransmitTime: transmitTime,
		Latitude:     payload.Latitude,
		Longitude:    payload.Longitude,
	}
	h.publish(hubmqtt.TopicMORaw(deviceID), 1, false, rawMsg)

	// Fragment reassembly: if payload is a fragment, collect and reassemble.
	if h.reassembler != nil && fragment.IsFragment(rawBytes) {
		reassembled, fragErr := h.reassembler.AddFragment(deviceID, rawBytes)
		if fragErr != nil {
			slog.Warn("astrocast: fragment error", "error", fragErr, "device", deviceID, "msg", payload.MessageGUID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_error"})
			return
		}
		if reassembled == nil {
			_, _, msgID := fragment.DecodeHeader(rawBytes[0], rawBytes[1])
			slog.Info("astrocast: fragment buffered, waiting for more",
				"device", deviceID, "msg", payload.MessageGUID, "msg_id", msgID,
				"pending", h.reassembler.PendingCount(),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_buffered"})
			return
		}
		slog.Info("astrocast: message reassembled from fragments",
			"device", deviceID, "bytes", len(reassembled),
		)
		rawBytes = reassembled
		rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
	}

	// Attempt E2E decryption if payload is large enough to be GCM-encrypted.
	encrypted := false
	if h.keyStore != nil && len(rawBytes) >= hubcrypto.Overhead {
		decrypted, err := h.keyStore.DecryptMessage(deviceID, rawBytes)
		if err == nil {
			slog.Info("astrocast: message decrypted",
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
	} else {
		if isPrintable(rawBytes) {
			text = string(rawBytes)
		}
	}

	// Publish decoded message to mo/decoded.
	decoded := MOMessage{
		DeviceGUID:   payload.DeviceGUID,
		MessageGUID:  payload.MessageGUID,
		Channel:      "astrocast",
		Text:         text,
		Raw:          rawB64,
		Compressed:   compressed,
		Compression:  compressionType,
		Encrypted:    encrypted,
		TransmitTime: transmitTime,
		Latitude:     payload.Latitude,
		Longitude:    payload.Longitude,
	}
	h.publish(hubmqtt.TopicMODecoded(deviceID), 1, false, decoded)

	// Publish position if lat/lon are present and non-zero.
	if payload.Latitude != 0 || payload.Longitude != 0 {
		pos := positionMessage{
			Lat:       payload.Latitude,
			Lon:       payload.Longitude,
			Source:    "astrocast",
			Timestamp: transmitTime,
		}
		h.publish(hubmqtt.TopicPosition(deviceID), 1, true, pos)
	}

	// Dead man's switch: device sent an MO message, reset its timer.
	if h.deadman != nil {
		h.deadman.CheckIn(deviceID)
	}

	// Audit: log message_received event.
	if h.audit != nil {
		tid := auth.TenantIDFromContext(r.Context())
		detail := fmt.Sprintf("device=%s message=%s bytes=%d channel=astrocast", payload.DeviceGUID, payload.MessageGUID, len(rawBytes))
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = strings.Split(fwd, ",")[0]
		}
		if err := h.audit.Log(r.Context(), tid, "message_received", "webhook", detail, ip); err != nil {
			slog.Warn("audit: failed to log message_received", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// verifySignature checks the HMAC-SHA256 signature from Astrocast webhook headers.
// Astrocast signs the raw request body with the shared secret and sends it
// in the X-Signature header as a hex-encoded HMAC-SHA256 digest.
func (h *Handler) verifySignature(r *http.Request) bool {
	sig := r.Header.Get("X-Signature")
	if sig == "" {
		return false
	}

	// We need to read the body for HMAC verification but also need it for JSON decode.
	// Since MaxBytesReader is already applied, we read, verify, then replace the body.
	bodyBytes, err := readBody(r)
	if err != nil {
		slog.Warn("astrocast: read body for signature check failed", "error", err)
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
