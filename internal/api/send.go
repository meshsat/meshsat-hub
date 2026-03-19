package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/compress"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/rock7"
	"github.com/cubeos-app/meshsat-hub/internal/sms"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// SendHandler handles MT message and SMS send requests.
type SendHandler struct {
	rock7Client *rock7.Client
	smsClient   *sms.Client
	store       store.Store
	keyStore    *hubcrypto.KeyStore
}

// NewSendHandler creates a new MT send handler.
func NewSendHandler(r7 *rock7.Client, s store.Store) *SendHandler {
	return &SendHandler{rock7Client: r7, store: s}
}

// SetKeyStore enables E2E encryption for MT messages.
func (h *SendHandler) SetKeyStore(ks *hubcrypto.KeyStore) {
	h.keyStore = ks
}

// SetSMSClient enables SMS sending.
func (h *SendHandler) SetSMSClient(c *sms.Client) {
	h.smsClient = c
}

type sendMessageRequest struct {
	Text     string `json:"text"`
	Compress bool   `json:"compress"` // SMAZ2 compress before sending (default: true)
	Encrypt  bool   `json:"encrypt"`  // AES-256-GCM encrypt with device key (default: true if key exists)
}

// SendMessage sends an MT message to a device via Rock7/Iridium.
//
//	@Summary      Send MT message to device
//	@Tags         devices
//	@Accept       json
//	@Produce      json
//	@Param        imei  path  string              true  "Device IMEI"
//	@Param        body  body  sendMessageRequest  true  "Message to send"
//	@Success      200   {object}  map[string]interface{}
//	@Failure      400   {object}  map[string]string
//	@Failure      502   {object}  map[string]string
//	@Router       /api/devices/{imei}/send [post]
func (h *SendHandler) SendMessage(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	var req sendMessageRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	if h.rock7Client == nil {
		writeError(w, http.StatusServiceUnavailable, "MT send not configured (set HUB_ROCK7_USERNAME)")
		return
	}

	// Build payload: text → compress → encrypt → hex
	payload := []byte(req.Text)
	compressed := false
	encrypted := false
	originalSize := len(payload)

	// Step 1: SMAZ2 compression (default on unless explicitly disabled)
	if req.Compress || req.Text != "" {
		smaz := compress.Compress(payload)
		if len(smaz) > 0 && len(smaz) < len(payload) {
			payload = smaz
			compressed = true
			slog.Info("send: compressed", "imei", imei, "original", originalSize, "compressed", len(payload))
		}
	}

	// Step 2: AES-256-GCM encryption (if device key exists)
	if h.keyStore != nil {
		ct, encErr := h.keyStore.EncryptMessage(imei, payload)
		if encErr == nil {
			payload = ct
			encrypted = true
			slog.Info("send: encrypted", "imei", imei, "bytes", len(payload))
		} else {
			slog.Debug("send: no encryption key for device", "imei", imei)
		}
	}

	dataHex := hex.EncodeToString(payload)
	result, err := h.rock7Client.SendMT(r.Context(), imei, dataHex)

	// Persist the MT message.
	tid := auth.TenantIDFromContext(r.Context())
	status := "queued"
	errMsg := ""
	mtID := ""
	if err != nil {
		status = "failed"
		errMsg = err.Error()
		slog.Error("send: MT failed", "imei", imei, "error", err)
	} else {
		mtID = result.MTID
		slog.Info("send: MT queued", "imei", imei, "mt_id", mtID,
			"original_bytes", originalSize, "wire_bytes", len(payload),
			"compressed", compressed, "encrypted", encrypted)
	}

	msg := &store.Message{
		ID:         "mt-" + mtID,
		DeviceIMEI: imei,
		Direction:  "mt",
		Channel:    "iridium",
		Text:       req.Text,
		RawHex:     dataHex,
		Compressed: compressed,
		Status:     status,
		Error:      errMsg,
	}
	if msg.ID == "mt-" {
		msg.ID = "mt-" + time.Now().Format("20060102150405")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.store.InsertMessage(ctx, tid, msg)

	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"status": "failed",
			"error":  errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "queued",
		"mt_id":          mtID,
		"imei":           imei,
		"compressed":     compressed,
		"encrypted":      encrypted,
		"original_bytes": originalSize,
		"wire_bytes":     len(payload),
	})
}

type sendSMSRequest struct {
	To       string `json:"to"` // E.164 phone number
	Text     string `json:"text"`
	Compress bool   `json:"compress"` // SMAZ2 compress (default false for SMS — human-readable)
	Encrypt  bool   `json:"encrypt"`  // AES-256-GCM encrypt (base64-encoded in SMS body)
}

// SendSMS sends an SMS message via Twilio.
//
//	@Summary      Send SMS message
//	@Tags         sms
//	@Accept       json
//	@Produce      json
//	@Param        body  body  sendSMSRequest  true  "SMS parameters"
//	@Success      200   {object}  map[string]interface{}
//	@Failure      400   {object}  map[string]string
//	@Failure      502   {object}  map[string]string
//	@Router       /api/sms/send [post]
func (h *SendHandler) SendSMS(w http.ResponseWriter, r *http.Request) {
	var req sendSMSRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.To == "" || req.Text == "" {
		writeError(w, http.StatusBadRequest, "to and text are required")
		return
	}
	if h.smsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "SMS not configured (set HUB_SMS_ACCOUNT_SID)")
		return
	}

	payload := []byte(req.Text)
	originalSize := len(payload)
	compressed := false
	encrypted := false
	finalBody := req.Text

	// Step 1: SMAZ2 compression (opt-in for SMS — usually want human-readable)
	if req.Compress {
		smaz := compress.Compress(payload)
		if len(smaz) > 0 {
			// Always compress if checkbox is on (even if same size — it's binary either way with encryption)
			payload = smaz
			compressed = true
		}
	}

	// Step 2: AES-256-GCM encryption (opt-in)
	if req.Encrypt && h.keyStore != nil {
		// Try recipient-specific key first, then global "sms" key.
		// Auto-generate "sms" key on first encrypted send.
		keyID := req.To // per-recipient key
		ct, encErr := h.keyStore.EncryptMessage(keyID, payload)
		if encErr != nil {
			// Try global "sms" key
			ct, encErr = h.keyStore.EncryptMessage("sms", payload)
		}
		if encErr != nil {
			// Auto-generate global SMS key
			_, _, genErr := h.keyStore.GenerateAndStore("sms", "decrypt")
			if genErr == nil {
				ct, encErr = h.keyStore.EncryptMessage("sms", payload)
				slog.Info("sms: auto-generated encryption key", "key_id", "sms")
			}
		}
		if encErr == nil {
			payload = ct
			encrypted = true
		}
	}

	// If encrypted, base64-encode for SMS transport (matches Android AesGcmCrypto format).
	// If only compressed (no encryption), use MSMS: hex prefix.
	if encrypted {
		finalBody = base64.StdEncoding.EncodeToString(payload) // pure base64 = Android-compatible
	} else if compressed {
		finalBody = "MSMS:" + hex.EncodeToString(payload) // compressed-only fallback
	}

	result, err := h.smsClient.Send(r.Context(), req.To, finalBody)

	tid := auth.TenantIDFromContext(r.Context())
	var status, errMsg, smsSID string
	if err != nil {
		status = "failed"
		errMsg = err.Error()
		slog.Error("sms: send failed", "to", req.To, "error", err)
	} else {
		smsSID = result.SID
		status = result.Status
		slog.Info("sms: sent", "to", req.To, "sid", smsSID,
			"original_bytes", originalSize, "wire_bytes", len(payload),
			"compressed", compressed, "encrypted", encrypted)
	}

	msg := &store.Message{
		ID:         "sms-" + smsSID,
		DeviceIMEI: req.To, // use phone number as device identifier
		Direction:  "mt",
		Channel:    "sms",
		Text:       req.Text,
		Compressed: compressed,
		Status:     status,
		Error:      errMsg,
	}
	if msg.ID == "sms-" {
		msg.ID = "sms-" + time.Now().Format("20060102150405")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.store.InsertMessage(ctx, tid, msg)

	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"status": "failed",
			"error":  errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         status,
		"sid":            smsSID,
		"to":             req.To,
		"compressed":     compressed,
		"encrypted":      encrypted,
		"original_bytes": originalSize,
		"wire_bytes":     len(finalBody),
	})
}
