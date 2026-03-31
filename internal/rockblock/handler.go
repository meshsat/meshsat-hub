package rockblock

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
	"strconv"
	"strings"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/codec"
	"github.com/cubeos-app/meshsat-hub/internal/compress"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/deadman"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/msvqsc"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// MOMessage represents a decoded Mobile Originated SBD message.
type MOMessage struct {
	IMEI             string  `json:"imei"`
	MOMSN            int     `json:"momsn"`
	Channel          string  `json:"channel"`
	Text             string  `json:"text,omitempty"`
	Raw              string  `json:"raw"`
	Compressed       bool    `json:"compressed"`
	Compression      string  `json:"compression,omitempty"`
	Encrypted        bool    `json:"encrypted"`
	TransmitTime     string  `json:"transmit_time"`
	IridiumLatitude  float64 `json:"iridium_latitude,omitempty"`
	IridiumLongitude float64 `json:"iridium_longitude,omitempty"`
	IridiumCEP       float64 `json:"iridium_cep,omitempty"`
}

// RawMessage is the raw MO payload published to mo/raw.
type RawMessage struct {
	IMEI             string  `json:"imei"`
	MOMSN            int     `json:"momsn"`
	Raw              string  `json:"raw"`
	Channel          string  `json:"channel"`
	TransmitTime     string  `json:"transmit_time"`
	IridiumLatitude  float64 `json:"iridium_latitude,omitempty"`
	IridiumLongitude float64 `json:"iridium_longitude,omitempty"`
	IridiumCEP       float64 `json:"iridium_cep,omitempty"`
}

// PositionMessage is published to the position topic when lat/lon are present.
type PositionMessage struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Alt       float64 `json:"alt,omitempty"`
	Speed     float64 `json:"speed,omitempty"`   // m/s
	Heading   float64 `json:"heading,omitempty"` // degrees 0-360
	Sats      int     `json:"sats,omitempty"`    // satellites in view
	CEP       float64 `json:"cep,omitempty"`     // circular error probable (meters)
	Source    string  `json:"source"`
	Timestamp string  `json:"timestamp"`
}

// Handler handles RockBLOCK/Ground Control webhook POST requests.
// reticulumReceiver is the interface for injecting inbound Reticulum packets.
type reticulumReceiver interface {
	OnReceive(raw []byte)
}

type Handler struct {
	mqtt            bus.MessageBus
	secret          string
	audit           *audit.Service
	dedup           dedup.Dedup
	reassembler     *fragment.Reassembler
	keyStore        *hubcrypto.KeyStore
	deadman         *deadman.Monitor
	msvqsc          *msvqsc.Decoder
	retIface        reticulumReceiver
	hembReassembler interface{ AddRawFrame([]byte) ([]byte, error) }
	store           interface {
		InsertMessage(ctx context.Context, tenantID string, m *store.Message) error
		SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
		SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
	}
}

// NewHandler creates a new RockBLOCK webhook handler.
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

// SetStore attaches a store for persisting MO messages and bridge state.
func (h *Handler) SetStore(s interface {
	InsertMessage(ctx context.Context, tenantID string, m *store.Message) error
	SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
	SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
}) {
	h.store = s
}

// SetReticulumIface attaches a Reticulum interface for forwarding raw packets.
func (h *Handler) SetReticulumIface(iface reticulumReceiver) {
	h.retIface = iface
}

// SetHeMBReassembler attaches a HeMB reassembly buffer for inbound coded symbols.
func (h *Handler) SetHeMBReassembler(r interface{ AddRawFrame([]byte) ([]byte, error) }) {
	h.hembReassembler = r
}

func (h *Handler) publish(topic string, qos byte, retained bool, v any) {
	if h.mqtt == nil {
		return
	}
	if err := h.mqtt.PublishJSON(topic, qos, retained, v); err != nil {
		slog.Error("rockblock: mqtt publish failed", "error", err, "topic", topic)
	}
}

// ServeHTTP handles the webhook POST from Ground Control.
//
//	@Summary      Receive RockBLOCK MO message
//	@Description  Ground Control posts MO SBD messages here
//	@Tags         webhook
//	@Accept       application/x-www-form-urlencoded
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Router       /api/webhook/rockblock [post]
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 1MB (SBD payloads are max 340 bytes, form overhead is minimal)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if err := r.ParseForm(); err != nil {
		slog.Warn("rockblock: parse form failed", "error", err)
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	// Verify shared secret — reject unsigned requests when secret is not configured.
	if h.secret == "" {
		slog.Warn("rockblock: webhook secret not configured, rejecting unsigned request")
		http.Error(w, `{"error":"webhook secret not configured"}`, http.StatusForbidden)
		return
	}
	if !h.verifySignature(r) {
		slog.Warn("rockblock: signature verification failed")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	imei := r.FormValue("imei")
	if imei == "" {
		http.Error(w, `{"error":"missing imei"}`, http.StatusBadRequest)
		return
	}

	momsn, _ := strconv.Atoi(r.FormValue("momsn"))
	transmitTime := r.FormValue("transmit_time")
	iridiumLat, _ := strconv.ParseFloat(r.FormValue("iridium_latitude"), 64)
	iridiumLon, _ := strconv.ParseFloat(r.FormValue("iridium_longitude"), 64)
	iridiumCEP, _ := strconv.ParseFloat(r.FormValue("iridium_cep"), 64)
	dataHex := r.FormValue("data")

	// Hex-decode the payload.
	rawBytes, err := hex.DecodeString(strings.ToLower(dataHex))
	if err != nil {
		slog.Warn("rockblock: hex decode failed", "error", err, "imei", imei)
		http.Error(w, `{"error":"invalid hex data"}`, http.StatusBadRequest)
		return
	}

	// Dedup: check if this message has already been processed.
	// Key: imei:momsn:sha256(data) — covers retransmissions from Ground Control.
	if h.dedup != nil {
		dedupHash := sha256.Sum256(rawBytes)
		dedupKey := fmt.Sprintf("%s:%d:%x", imei, momsn, dedupHash[:8])
		if !h.dedup.IsNew(dedupKey) {
			slog.Info("rockblock: duplicate MO suppressed", "imei", imei, "momsn", momsn)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
			return
		}
	}

	// HeMB frame detection — intercept before other processing.
	if protocol.IsHeMBFrame(rawBytes) {
		slog.Info("rockblock: HeMB frame detected", "imei", imei, "bytes", len(rawBytes))
		if h.hembReassembler != nil {
			if decoded, err := h.hembReassembler.AddRawFrame(rawBytes); err != nil {
				slog.Warn("rockblock: HeMB reassembly error", "error", err, "imei", imei)
			} else if decoded != nil {
				slog.Info("rockblock: HeMB decoded", "imei", imei, "payload_bytes", len(decoded))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "hemb_symbol"})
		return
	}

	// Check if this is a bridge satellite uplink message (magic 0x4D53 "MS").
	// These are sent by field bridges when internet is down, encoding position/SOS/health
	// as compact binary for relay to the Hub via Iridium satellite.
	if h.handleBridgeSatUplink(r.Context(), imei, rawBytes) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "bridge_sat_uplink"})
		return
	}

	// Check if this is a Reticulum packet and forward to the relay.
	// Reticulum packets have a minimum header size of 19 bytes (Type 1).
	if h.retIface != nil && len(rawBytes) >= 19 {
		h.retIface.OnReceive(rawBytes)
	}

	rawB64 := base64.StdEncoding.EncodeToString(rawBytes)

	slog.Info("rockblock: MO received",
		"imei", imei,
		"momsn", momsn,
		"bytes", len(rawBytes),
		"transmit_time", transmitTime,
	)

	// Publish raw payload to mo/raw.
	rawMsg := RawMessage{
		IMEI:             imei,
		MOMSN:            momsn,
		Raw:              rawB64,
		Channel:          "iridium",
		TransmitTime:     transmitTime,
		IridiumLatitude:  iridiumLat,
		IridiumLongitude: iridiumLon,
		IridiumCEP:       iridiumCEP,
	}
	h.publish(hubmqtt.TopicMORaw(imei), 1, false, rawMsg)

	// Fragment reassembly: if payload is a fragment, collect and reassemble.
	if h.reassembler != nil && fragment.IsFragment(rawBytes) {
		reassembled, fragErr := h.reassembler.AddFragment(imei, rawBytes)
		if fragErr != nil {
			slog.Warn("rockblock: fragment error", "error", fragErr, "imei", imei, "momsn", momsn)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_error"})
			return
		}
		if reassembled == nil {
			_, _, msgID := fragment.DecodeHeader(rawBytes[0], rawBytes[1])
			slog.Info("rockblock: fragment buffered, waiting for more",
				"imei", imei, "momsn", momsn, "msg_id", msgID,
				"pending", h.reassembler.PendingCount(),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "fragment_buffered"})
			return
		}
		slog.Info("rockblock: message reassembled from fragments",
			"imei", imei, "bytes", len(reassembled),
		)
		rawBytes = reassembled
		rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
	}

	// Strip protocol version byte (if present) before further processing.
	protoVersion, strippedBytes := codec.StripVersionByte(rawBytes)
	if protoVersion > 0 {
		slog.Info("rockblock: protocol version detected",
			"imei", imei, "version", protoVersion)
		rawBytes = strippedBytes
	} else {
		slog.Debug("rockblock: legacy message (no version byte)", "imei", imei)
	}
	if protoVersion > 0 && protoVersion != codec.ProtoVersion1 {
		slog.Warn("rockblock: protocol version mismatch, processing anyway",
			"imei", imei, "version", protoVersion, "expected", codec.ProtoVersion1)
	}

	// Attempt E2E decryption if payload is large enough to be GCM-encrypted
	// (12-byte nonce + ciphertext + 16-byte tag = 28 bytes minimum overhead).
	encrypted := false
	if h.keyStore != nil && len(rawBytes) >= hubcrypto.Overhead {
		decrypted, err := h.keyStore.DecryptMessage(imei, rawBytes)
		if err == nil {
			slog.Info("rockblock: message decrypted",
				"imei", imei, "encrypted_bytes", len(rawBytes), "decrypted_bytes", len(decrypted))
			rawBytes = decrypted
			rawB64 = base64.StdEncoding.EncodeToString(rawBytes)
			encrypted = true
		}
		// If decryption fails, the payload is either not encrypted or uses
		// an unknown key — proceed with the raw bytes (backwards compatible).
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
		// Try MSVQ-SC semantic decompression (Android).
		decoded, decErr := h.msvqsc.Decode(rawBytes)
		if decErr == nil && decoded != "" {
			text = decoded
			compressed = true
			compressionType = "msvqsc"
			slog.Info("rockblock: MSVQ-SC decoded", "imei", imei, "text", text)
		}
	} else {
		// Not compressed or not valid text — try raw as UTF-8.
		if isPrintable(rawBytes) {
			text = string(rawBytes)
		}
	}

	// Publish decoded message to mo/decoded.
	decoded := MOMessage{
		IMEI:             imei,
		MOMSN:            momsn,
		Channel:          "iridium",
		Text:             text,
		Raw:              rawB64,
		Compressed:       compressed,
		Compression:      compressionType,
		Encrypted:        encrypted,
		TransmitTime:     transmitTime,
		IridiumLatitude:  iridiumLat,
		IridiumLongitude: iridiumLon,
		IridiumCEP:       iridiumCEP,
	}
	h.publish(hubmqtt.TopicMODecoded(imei), 1, false, decoded)

	// Persist MO message to database.
	if h.store != nil {
		tid := auth.TenantIDFromContext(r.Context())
		msg := &store.Message{
			ID:         fmt.Sprintf("mo-%d", time.Now().UnixNano()),
			DeviceIMEI: imei,
			Direction:  "mo",
			Channel:    "iridium",
			MOMSN:      momsn,
			Text:       text,
			RawHex:     rawB64,
			Compressed: compressed,
			Status:     "received",
			Lat:        iridiumLat,
			Lon:        iridiumLon,
		}
		if err := h.store.InsertMessage(r.Context(), tid, msg); err != nil {
			slog.Warn("rockblock: message persist failed", "error", err, "imei", imei)
		} else {
			slog.Info("rockblock: message persisted", "imei", imei, "momsn", momsn)
		}
	}

	// Publish position if lat/lon are present and non-zero.
	if iridiumLat != 0 || iridiumLon != 0 {
		ts := transmitTime
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		pos := PositionMessage{
			Lat:       iridiumLat,
			Lon:       iridiumLon,
			CEP:       iridiumCEP,
			Source:    "iridium_cep",
			Timestamp: ts,
		}
		h.publish(hubmqtt.TopicPosition(imei), 1, true, pos)
	}

	// Dead man's switch: device sent an MO message, reset its timer.
	if h.deadman != nil {
		h.deadman.CheckIn(imei)
	}

	// Audit: log message_received event (use RemoteAddr directly — never trust X-Forwarded-For in webhook handlers).
	if h.audit != nil {
		tid := auth.TenantIDFromContext(r.Context())
		detail := fmt.Sprintf("imei=%s momsn=%d bytes=%d", imei, momsn, len(rawBytes))
		ip := r.RemoteAddr
		if err := h.audit.Log(r.Context(), tid, "message_received", "webhook", detail, ip); err != nil {
			slog.Warn("audit: failed to log message_received", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleBridgeSatUplink checks if the raw bytes are a bridge satellite uplink
// message (magic 0x4D53) and processes it. Returns true if handled.
func (h *Handler) handleBridgeSatUplink(ctx context.Context, imei string, rawBytes []byte) bool {
	if !bridge.IsBridgeSatUplink(rawBytes) {
		return false
	}

	msgType, payload, err := bridge.DecodeSatUplink(rawBytes)
	if err != nil {
		slog.Warn("rockblock: bridge sat uplink decode failed", "error", err, "imei", imei)
		return false
	}

	switch msgType {
	case bridge.SatMsgPosition:
		bridgeID, lat, lon, alt, _, ts, err := bridge.DecodeSatPosition(payload)
		if err != nil {
			slog.Warn("rockblock: bridge sat position decode failed", "error", err, "imei", imei)
			return true
		}
		slog.Info("rockblock: bridge sat uplink position",
			"bridge_id", bridgeID, "lat", lat, "lon", lon, "alt", alt, "imei", imei)
		pos := PositionMessage{
			Lat:       lat,
			Lon:       lon,
			Alt:       float64(alt),
			Source:    "satellite_uplink",
			Timestamp: ts.Format(time.RFC3339),
		}
		h.publish(hubmqtt.TopicPosition(bridgeID), 1, true, pos)
		// Mark bridge as online via satellite.
		if h.store != nil {
			tid := auth.TenantIDFromContext(ctx)
			_ = h.store.SetBridgeOnline(ctx, tid, bridgeID, true)
		}

	case bridge.SatMsgSOS:
		bridgeID, deviceID, lat, lon, message, ts, err := bridge.DecodeSatSOS(payload)
		if err != nil {
			slog.Warn("rockblock: bridge sat SOS decode failed", "error", err, "imei", imei)
			return true
		}
		slog.Warn("rockblock: BRIDGE SOS via satellite",
			"bridge_id", bridgeID, "device_id", deviceID, "lat", lat, "lon", lon, "message", message)
		sos := map[string]interface{}{
			"bridge_id": bridgeID,
			"device_id": deviceID,
			"lat":       lat,
			"lon":       lon,
			"message":   message,
			"source":    "satellite_uplink",
			"timestamp": ts.Format(time.RFC3339),
		}
		h.publish(hubmqtt.TopicSOS(bridgeID), 1, false, sos)

	case bridge.SatMsgHealthSummary:
		bridgeID, uptimeSec, cpuPct, memPct, diskPct, ifaces, ts, err := bridge.DecodeSatHealth(payload)
		if err != nil {
			slog.Warn("rockblock: bridge sat health decode failed", "error", err, "imei", imei)
			return true
		}
		slog.Info("rockblock: bridge sat uplink health",
			"bridge_id", bridgeID, "uptime", uptimeSec, "cpu", cpuPct, "mem", memPct, "disk", diskPct,
			"interfaces", len(ifaces), "timestamp", ts)
		if h.store != nil {
			tid := auth.TenantIDFromContext(ctx)
			healthJSON, _ := json.Marshal(map[string]interface{}{
				"uptime_sec": uptimeSec,
				"cpu_pct":    cpuPct,
				"mem_pct":    memPct,
				"disk_pct":   diskPct,
				"interfaces": len(ifaces),
				"source":     "satellite_uplink",
				"timestamp":  ts.Format(time.RFC3339),
			})
			_ = h.store.SetBridgeHealth(ctx, tid, bridgeID, string(healthJSON))
			_ = h.store.SetBridgeOnline(ctx, tid, bridgeID, true)
		}

	default:
		slog.Warn("rockblock: unknown bridge sat uplink type", "type", msgType, "imei", imei)
	}

	// Audit: log bridge satellite uplink event.
	if h.audit != nil {
		tid := auth.TenantIDFromContext(ctx)
		detail := fmt.Sprintf("imei=%s type=0x%02x bytes=%d", imei, msgType, len(rawBytes))
		_ = h.audit.Log(ctx, tid, "bridge_sat_uplink", "webhook", detail, "")
	}

	return true
}

// verifySignature checks the HMAC-SHA256 signature if present,
// or falls back to checking the JWT query parameter.
func (h *Handler) verifySignature(r *http.Request) bool {
	// Check for HMAC in form data.
	sig := r.FormValue("JWT")
	if sig != "" {
		// Ground Control JWT verification — constant-time shared-secret check.
		return hmac.Equal([]byte(sig), []byte(h.secret))
	}

	// Check X-Hub-Signature header (HMAC-SHA256).
	headerSig := r.Header.Get("X-Hub-Signature")
	if headerSig != "" {
		mac := hmac.New(sha256.New, []byte(h.secret))
		mac.Write([]byte(r.FormValue("data")))
		expected := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(headerSig), []byte(expected))
	}

	// No signature provided — reject if secret is configured.
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
