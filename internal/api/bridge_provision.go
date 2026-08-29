package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// ProvisionBundle contains everything a bridge/Android app needs to connect.
// Returned by the nonce-authenticated claim endpoint, NOT embedded in the QR.
type ProvisionBundle struct {
	Version             string `json:"v"`            // "1"
	BridgeID            string `json:"bid"`          // bridge identifier
	MQTTURL             string `json:"mqtt"`         // wss://mqtt-hub.meshsat.net/mqtt
	Username            string `json:"user"`         // MQTT username (always "meshsat" for NATS auth)
	Password            string `json:"pass"`         // MQTT password (shared NATS auth password)
	CertPEM             string `json:"cert"`         // client TLS certificate
	KeyPEM              string `json:"key"`          // client TLS private key (one-time)
	CaPEM               string `json:"ca"`           // CA certificate
	CertExpires         string `json:"cert_exp"`     // certificate expiry (RFC3339)
	ReticulumTCP        string `json:"ret_tcp"`      // Reticulum TCP peer
	DirectorySigningPub []byte `json:"dir_sign_pub"` // Hub's ECDSA-P256 directory-signing pubkey (PKIX DER) — bridge pins on first provision [MESHSAT-539]
}

// provisionStash holds pre-generated credentials waiting to be claimed.
// Stored as JSON in system_config with key "provision_stash:{bridge_id}".
type provisionStash struct {
	Nonce     string          `json:"nonce"`
	Bundle    ProvisionBundle `json:"bundle"`
	CreatedAt time.Time       `json:"created_at"`
}

// BridgeProvisionHandler provides QR-based provisioning.
type BridgeProvisionHandler struct {
	store       store.Store
	ca          *bridge.CertAuthority
	trustAnchor *directory.TrustAnchor
}

// NewBridgeProvisionHandler returns a handler that stashes credentials for
// nonce-authenticated claim. trustAnchor supplies the Hub's directory-signing
// pubkey published in the bundle (MESHSAT-539); it may be nil in test setups.
func NewBridgeProvisionHandler(s store.Store, ca *bridge.CertAuthority, trustAnchor *directory.TrustAnchor) *BridgeProvisionHandler {
	return &BridgeProvisionHandler{store: s, ca: ca, trustAnchor: trustAnchor}
}

// generateAndStash creates fresh credentials, stores them in a stash keyed
// by nonce, and returns the nonce. The full bundle is claimed via ClaimProvision.
func (h *BridgeProvisionHandler) generateAndStash(r *http.Request, id, tid string) (string, error) {
	// Generate single-use nonce (16 random bytes = 32 hex chars for security).
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Generate MQTT password.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	username := id
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hash)); err != nil {
		return "", fmt.Errorf("store credentials: %w", err)
	}

	mqttURL, _ := h.store.GetSystemConfig(r.Context(), mqttPublicURLKey)
	if mqttURL == "" {
		mqttURL = os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	}
	if mqttURL == "" {
		return "", fmt.Errorf("MQTT public URL not configured")
	}

	if h.ca == nil {
		return "", fmt.Errorf("certificate authority not configured")
	}

	certPEM, keyPEM, err := h.ca.IssueBridgeCert(id, 90)
	if err != nil {
		return "", fmt.Errorf("issue certificate: %w", err)
	}

	expiry := time.Now().Add(90 * 24 * time.Hour)
	if err := h.store.SetBridgeCertificate(r.Context(), tid, id, string(certPEM), expiry); err != nil {
		return "", fmt.Errorf("store certificate: %w", err)
	}

	retTCP := os.Getenv("MESHSAT_RETICULUM_PUBLIC_TCP")
	if retTCP == "" {
		retTCP = "reticulum.meshsat.net:443"
	}

	// NATS MQTT auth: bridges use shared username "meshsat" with the NATS password.
	// Per-bridge identity comes from MQTT client ID + mTLS certificate CN.
	natsMQTTPassword := os.Getenv("NATS_MQTT_PASSWORD")

	var dirSignPub []byte
	if h.trustAnchor != nil {
		dirSignPub = h.trustAnchor.PublicKey()
	}

	stash := provisionStash{
		Nonce: nonce,
		Bundle: ProvisionBundle{
			Version:             "1",
			BridgeID:            id,
			MQTTURL:             mqttURL,
			Username:            "meshsat",
			Password:            natsMQTTPassword,
			CertPEM:             string(certPEM),
			KeyPEM:              string(keyPEM),
			CaPEM:               string(h.ca.CACertPEM()),
			CertExpires:         expiry.Format(time.RFC3339),
			ReticulumTCP:        retTCP,
			DirectorySigningPub: dirSignPub,
		},
		CreatedAt: time.Now(),
	}

	stashJSON, err := json.Marshal(stash)
	if err != nil {
		return "", fmt.Errorf("marshal stash: %w", err)
	}

	// Store stash — overwrites any previous (invalidates old QRs).
	stashKey := "provision_stash:" + id
	if err := h.store.SetSystemConfig(r.Context(), stashKey, string(stashJSON)); err != nil {
		return "", fmt.Errorf("store stash: %w", err)
	}

	return nonce, nil
}

// Provision generates credentials and returns the full bundle (authenticated endpoint).
// @Summary One-step bridge provisioning
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} ProvisionBundle
// @Router /api/bridges/{id}/provision [post]
func (h *BridgeProvisionHandler) Provision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	nonce, err := h.generateAndStash(r, id, tid)
	if err != nil {
		slog.Error("bridge provision failed", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// For the direct API, return the full bundle immediately.
	stashKey := "provision_stash:" + id
	stashJSON, err := h.store.GetSystemConfig(r.Context(), stashKey)
	if err != nil || stashJSON == "" {
		writeError(w, http.StatusInternalServerError, "stash not found")
		return
	}

	var stash provisionStash
	if err := json.Unmarshal([]byte(stashJSON), &stash); err != nil {
		writeError(w, http.StatusInternalServerError, "corrupt stash")
		return
	}

	slog.Info("bridge provisioned (direct)", "bridge_id", id, "nonce", nonce[:8])
	writeJSON(w, http.StatusOK, stash.Bundle)
}

// ProvisionQR renders a QR code containing a short provisioning URL.
// The QR encodes: meshsat://provision/{bridge_id}/{nonce}?hub={hub_host}
// The app scans this, then fetches the full bundle from the Hub via the
// ClaimProvision endpoint (no auth needed — the nonce IS the auth).
// @Summary Generate provisioning QR code
// @Tags bridges
// @Produce image/png
// @Param id path string true "Bridge ID"
// @Param size query int false "QR code size in pixels (default 512)"
// @Success 200 {file} image/png
// @Router /api/bridges/{id}/provision/qr [post]
func (h *BridgeProvisionHandler) ProvisionQR(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	nonce, err := h.generateAndStash(r, id, tid)
	if err != nil {
		slog.Error("bridge provision QR failed", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// Hub public hostname for the claim URL.
	hubHost := os.Getenv("MESHSAT_HUB_PUBLIC_HOST")
	if hubHost == "" {
		hubHost = r.Host // fallback to request host
	}
	if hubHost == "" {
		hubHost = "hub.meshsat.net"
	}

	// QR content: ~100 bytes, fits easily in any QR code.
	// Format: meshsat://provision/{bridge_id}/{nonce}?hub={host}
	qrContent := fmt.Sprintf("meshsat://provision/%s/%s?hub=%s", id, nonce, hubHost)

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
		slog.Error("failed to generate QR code", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "QR code generation failed")
		return
	}

	slog.Info("provision QR generated",
		"bridge_id", id,
		"nonce", nonce[:8]+"...",
		"qr_content_len", len(qrContent),
		"qr_size", size)

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"meshsat-provision-%s.png\"", id))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ClaimProvision is the unauthenticated endpoint that the Android app calls
// after scanning the QR code. The nonce acts as a single-use bearer token.
// Returns the full ProvisionBundle and deletes the stash (one-time use).
// @Summary Claim provisioning bundle (no auth, nonce-authenticated)
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Param nonce path string true "Single-use provisioning nonce"
// @Success 200 {object} ProvisionBundle
// @Failure 404 {object} map[string]string "Invalid or expired nonce"
// @Router /api/bridges/{id}/provision/{nonce} [get]
func (h *BridgeProvisionHandler) ClaimProvision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	nonce := chi.URLParam(r, "nonce")

	stashKey := "provision_stash:" + id
	stashJSON, err := h.store.GetSystemConfig(r.Context(), stashKey)
	if err != nil || stashJSON == "" {
		writeError(w, http.StatusNotFound, "no pending provision for this bridge")
		return
	}

	var stash provisionStash
	if err := json.Unmarshal([]byte(stashJSON), &stash); err != nil {
		writeError(w, http.StatusInternalServerError, "corrupt provision data")
		return
	}

	// Verify nonce matches.
	if stash.Nonce != nonce {
		slog.Warn("provision claim: nonce mismatch",
			"bridge_id", id,
			"expected", stash.Nonce[:8]+"...",
			"got", nonce[:min(8, len(nonce))]+"...")
		writeError(w, http.StatusNotFound, "invalid or expired provisioning token")
		return
	}

	// Check age — reject if older than 30 minutes.
	if time.Since(stash.CreatedAt) > 30*time.Minute {
		// Clean up expired stash.
		_ = h.store.SetSystemConfig(r.Context(), stashKey, "")
		writeError(w, http.StatusGone, "provisioning token expired (>30 minutes)")
		return
	}

	// Delete the stash — single use.
	_ = h.store.SetSystemConfig(r.Context(), stashKey, "")

	slog.Info("provision claimed",
		"bridge_id", id,
		"nonce", nonce[:8]+"...",
		"age_sec", int(time.Since(stash.CreatedAt).Seconds()))

	writeJSON(w, http.StatusOK, stash.Bundle)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
