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
// to the Hub. This is the JSON payload embedded in the QR code.
type ProvisionBundle struct {
	Version      string `json:"v"`        // "1" — bundle version
	BridgeID     string `json:"bid"`      // bridge identifier
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

// Provision generates MQTT credentials + mTLS certificate in a single call
// and returns a JSON bundle suitable for QR encoding.
// @Summary One-step bridge provisioning
// @Description Generates MQTT credentials and mTLS certificate in one call. Returns a JSON bundle that can be QR-encoded for scanning by Android/bridge apps.
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

	// Generate MQTT password.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate password")
		return
	}
	password := hex.EncodeToString(passwordBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	username := id
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hash)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store credentials")
		return
	}

	mqttURL, _ := h.store.GetSystemConfig(r.Context(), mqttPublicURLKey)
	if mqttURL == "" {
		mqttURL = os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	}
	if mqttURL == "" {
		writeError(w, http.StatusInternalServerError, "MQTT public URL not configured")
		return
	}

	// Generate mTLS certificate.
	if h.ca == nil {
		writeError(w, http.StatusInternalServerError, "certificate authority not configured")
		return
	}

	certPEM, keyPEM, err := h.ca.IssueBridgeCert(id, 90)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to issue certificate: %v", err))
		return
	}

	expiry := time.Now().Add(90 * 24 * time.Hour)

	if err := h.store.SetBridgeCertificate(r.Context(), tid, id, string(certPEM), expiry); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store certificate")
		return
	}

	// Reticulum TCP peer — from env or default.
	retTCP := os.Getenv("MESHSAT_RETICULUM_PUBLIC_TCP")
	if retTCP == "" {
		retTCP = "reticulum.meshsat.net:443"
	}

	bundle := ProvisionBundle{
		Version:      "1",
		BridgeID:     id,
		MQTTURL:      mqttURL,
		Username:     username,
		Password:     password,
		CertPEM:      string(certPEM),
		KeyPEM:       string(keyPEM),
		CaPEM:        string(h.ca.CACertPEM()),
		CertExpires:  expiry.Format(time.RFC3339),
		ReticulumTCP: retTCP,
	}

	slog.Info("bridge provisioned (one-step)", "bridge_id", id, "cert_expires", expiry.Format(time.RFC3339))

	writeJSON(w, http.StatusOK, bundle)
}

// ProvisionQR renders the provision bundle as a QR code PNG image.
// The QR encodes: meshsat://provision/<base64url-encoded-json>
// @Summary Generate provisioning QR code
// @Description Generates MQTT credentials + mTLS certificate and returns a QR code PNG that the Android app can scan to auto-configure.
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

	// Generate the full provision bundle (reuse Provision logic).
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate password")
		return
	}
	password := hex.EncodeToString(passwordBytes)

	hashBytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	username := id
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hashBytes)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store credentials")
		return
	}

	mqttURL, _ := h.store.GetSystemConfig(r.Context(), mqttPublicURLKey)
	if mqttURL == "" {
		mqttURL = os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	}

	if h.ca == nil {
		writeError(w, http.StatusInternalServerError, "certificate authority not configured")
		return
	}

	certPEM, keyPEM, err := h.ca.IssueBridgeCert(id, 90)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to issue certificate: %v", err))
		return
	}

	expiry := time.Now().Add(90 * 24 * time.Hour)
	_ = h.store.SetBridgeCertificate(r.Context(), tid, id, string(certPEM), expiry)

	retTCP := os.Getenv("MESHSAT_RETICULUM_PUBLIC_TCP")
	if retTCP == "" {
		retTCP = "reticulum.meshsat.net:443"
	}

	bundle := ProvisionBundle{
		Version:      "1",
		BridgeID:     id,
		MQTTURL:      mqttURL,
		Username:     username,
		Password:     password,
		CertPEM:      string(certPEM),
		KeyPEM:       string(keyPEM),
		CaPEM:        string(h.ca.CACertPEM()),
		CertExpires:  expiry.Format(time.RFC3339),
		ReticulumTCP: retTCP,
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
		if n := 0; len(s) <= 4 {
			for _, c := range s {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}
			if n >= 128 && n <= 2048 {
				size = n
			}
		}
	}

	png, err := qrcode.Encode(qrContent, qrcode.Medium, size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to generate QR code: %v", err))
		return
	}

	slog.Info("bridge provisioned via QR", "bridge_id", id, "qr_size", size, "content_len", len(qrContent))

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"meshsat-provision-%s.png\"", id))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
