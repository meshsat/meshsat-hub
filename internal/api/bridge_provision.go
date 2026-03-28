package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// ProvisionBundle contains everything a bridge/Android app needs to connect
// to the Hub. Single-use: each POST /provision generates fresh credentials
// and a nonce. The previous password hash is overwritten, so old QR codes
// stop working immediately.
type ProvisionBundle struct {
	Version      string `json:"v"`        // "1" — bundle version
	BridgeID     string `json:"bid"`      // bridge identifier
	Nonce        string `json:"nonce"`    // single-use token (8 hex chars) — cleared on first MQTT birth
	MQTTURL      string `json:"mqtt"`     // wss://mqtt-hub.meshsat.net/mqtt
	Username     string `json:"user"`     // MQTT username
	Password     string `json:"pass"`     // MQTT password (plaintext, one-time)
	CertPEM      string `json:"cert"`     // client TLS certificate
	KeyPEM       string `json:"key"`      // client TLS private key (one-time)
	CaPEM        string `json:"ca"`       // CA certificate for server verification
	CertExpires  string `json:"cert_exp"` // certificate expiry (RFC3339)
	ReticulumTCP string `json:"ret_tcp"`  // Reticulum TCP peer (e.g. reticulum.meshsat.net:443)
}

// BridgeProvisionHandler provides QR-based provisioning for bridges and Android apps.
type BridgeProvisionHandler struct {
	store store.Store
	ca    *bridge.CertAuthority
}

// NewBridgeProvisionHandler creates a new provisioning handler.
func NewBridgeProvisionHandler(s store.Store, ca *bridge.CertAuthority) *BridgeProvisionHandler {
	return &BridgeProvisionHandler{store: s, ca: ca}
}

// generateBundle creates fresh MQTT credentials, mTLS cert, and a single-use
// nonce for the given bridge. The nonce is stored in system_config as
// "provision_nonce:{bridge_id}" and cleared when the bridge sends its first
// MQTT birth message (or on the next provision call, whichever comes first).
func (h *BridgeProvisionHandler) generateBundle(r *http.Request, id, tid string) (*ProvisionBundle, error) {
	// Generate single-use nonce (4 random bytes = 8 hex chars).
	nonceBytes := make([]byte, 4)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Store nonce — overwrites any previous nonce (invalidates old QRs).
	nonceKey := "provision_nonce:" + id
	if err := h.store.SetSystemConfig(r.Context(), nonceKey, nonce); err != nil {
		slog.Warn("provision: failed to store nonce", "bridge_id", id, "error", err)
	}

	// Generate MQTT password.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	username := id
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hash)); err != nil {
		return nil, fmt.Errorf("store credentials: %w", err)
	}

	mqttURL, _ := h.store.GetSystemConfig(r.Context(), mqttPublicURLKey)
	if mqttURL == "" {
		mqttURL = os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	}
	if mqttURL == "" {
		return nil, fmt.Errorf("MQTT public URL not configured")
	}

	if h.ca == nil {
		return nil, fmt.Errorf("certificate authority not configured")
	}

	certPEM, keyPEM, err := h.ca.IssueBridgeCert(id, 90)
	if err != nil {
		return nil, fmt.Errorf("issue certificate: %w", err)
	}

	expiry := time.Now().Add(90 * 24 * time.Hour)
	if err := h.store.SetBridgeCertificate(r.Context(), tid, id, string(certPEM), expiry); err != nil {
		return nil, fmt.Errorf("store certificate: %w", err)
	}

	retTCP := os.Getenv("MESHSAT_RETICULUM_PUBLIC_TCP")
	if retTCP == "" {
		retTCP = "reticulum.meshsat.net:443"
	}

	return &ProvisionBundle{
		Version:      "1",
		BridgeID:     id,
		Nonce:        nonce,
		MQTTURL:      mqttURL,
		Username:     username,
		Password:     password,
		CertPEM:      string(certPEM),
		KeyPEM:       string(keyPEM),
		CaPEM:        string(h.ca.CACertPEM()),
		CertExpires:  expiry.Format(time.RFC3339),
		ReticulumTCP: retTCP,
	}, nil
}

// Provision generates MQTT credentials + mTLS certificate in a single call
// and returns a JSON bundle suitable for QR encoding.
// @Summary One-step bridge provisioning
// @Description Generates MQTT credentials, mTLS certificate, and a single-use nonce. Returns a JSON bundle. Each call invalidates previous credentials.
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} ProvisionBundle
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id}/provision [post]
func (h *BridgeProvisionHandler) Provision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	bundle, err := h.generateBundle(r, id, tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	slog.Info("bridge provisioned",
		"bridge_id", id,
		"nonce", bundle.Nonce,
		"cert_expires", bundle.CertExpires)

	writeJSON(w, http.StatusOK, bundle)
}

// ProvisionQR renders the provision bundle as a QR code PNG image.
// The QR encodes: meshsat://provision/<base64url-encoded-json>
// @Summary Generate provisioning QR code
// @Description Generates credentials + certificate and returns a QR code PNG. Single-use: each call invalidates previous credentials.
// @Tags bridges
// @Produce image/png
// @Param id path string true "Bridge ID"
// @Param size query int false "QR code size in pixels (default 512)"
// @Success 200 {file} image/png
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id}/provision/qr [post]
func (h *BridgeProvisionHandler) ProvisionQR(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	bundle, err := h.generateBundle(r, id, tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal bundle")
		return
	}

	// Encode as meshsat://provision/<base64url>
	qrContent := "meshsat://provision/" + base64.RawURLEncoding.EncodeToString(bundleJSON)

	// QR code size.
	size := 512
	if s := r.URL.Query().Get("size"); s != "" {
		n := 0
		for _, c := range s {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		if n >= 128 && n <= 2048 {
			size = n
		}
	}

	png, err := qrcode.Encode(qrContent, qrcode.Medium, size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to generate QR code: %v", err))
		return
	}

	slog.Info("bridge provisioned via QR",
		"bridge_id", id,
		"nonce", bundle.Nonce,
		"qr_size", size,
		"content_len", len(qrContent))

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"meshsat-provision-%s.png\"", id))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
