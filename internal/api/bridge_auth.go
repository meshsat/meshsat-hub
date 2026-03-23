package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// BridgeAuthHandler provides REST endpoints for bridge MQTT authentication.
type BridgeAuthHandler struct {
	store store.Store
	ca    *bridge.CertAuthority
}

// NewBridgeAuthHandler creates a new bridge authentication API handler.
func NewBridgeAuthHandler(s store.Store, ca *bridge.CertAuthority) *BridgeAuthHandler {
	return &BridgeAuthHandler{store: s, ca: ca}
}

// credentialResponse is the one-time response containing MQTT credentials.
type credentialResponse struct {
	BridgeID string `json:"bridge_id"`
	Username string `json:"username"`
	Password string `json:"password"`
	MQTTURL  string `json:"mqtt_url"`
}

// certificateResponse is the one-time response containing TLS certificate data.
type certificateResponse struct {
	BridgeID string `json:"bridge_id"`
	CertPEM  string `json:"cert_pem"`
	KeyPEM   string `json:"key_pem"`
	CaPEM    string `json:"ca_pem"`
	Expires  string `json:"expires"`
}

// aclRegenResponse is the response from ACL regeneration.
type aclRegenResponse struct {
	BridgesConfigured int `json:"bridges_configured"`
}

// GenerateCredentials creates MQTT credentials for a bridge.
// @Summary Generate MQTT credentials for a bridge
// @Description Generates a random password and stores the bcrypt hash. Returns the plaintext password once — it is never stored.
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} credentialResponse
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id}/credentials [post]
func (h *BridgeAuthHandler) GenerateCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	// Generate 32-byte random password (64 hex chars).
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate password")
		return
	}
	password := hex.EncodeToString(passwordBytes)

	// Hash with bcrypt (cost 10).
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	username := id // MQTT username = bridge ID
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hash)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store credentials")
		return
	}

	mqttURL := os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	if mqttURL == "" {
		mqttURL = "mqtt://hub.meshsat.net:6071"
	}

	writeJSON(w, http.StatusOK, credentialResponse{
		BridgeID: id,
		Username: username,
		Password: password,
		MQTTURL:  mqttURL,
	})
}

// IssueCertificate generates a TLS client certificate for a bridge.
// @Summary Issue TLS client certificate for a bridge
// @Description Generates an ECDSA P-256 client certificate. Returns the private key once — it is never stored server-side.
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} certificateResponse
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id}/certificate [post]
func (h *BridgeAuthHandler) IssueCertificate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
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

	// Store cert (NOT key) in database.
	if err := h.store.SetBridgeCertificate(r.Context(), tid, id, string(certPEM), expiry); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store certificate")
		return
	}

	writeJSON(w, http.StatusOK, certificateResponse{
		BridgeID: id,
		CertPEM:  string(certPEM),
		KeyPEM:   string(keyPEM),
		CaPEM:    string(h.ca.CACertPEM()),
		Expires:  expiry.Format(time.RFC3339),
	})
}

// RegenerateACL regenerates Mosquitto password and ACL files from bridge credentials.
// @Summary Regenerate Mosquitto ACL files
// @Description Reads all bridges with credentials and generates Mosquitto password and ACL files.
// @Tags bridges
// @Produce json
// @Success 200 {object} aclRegenResponse
// @Failure 500 {object} map[string]string
// @Router /api/bridges/acl/regenerate [post]
func (h *BridgeAuthHandler) RegenerateACL(w http.ResponseWriter, r *http.Request) {
	bridges, err := h.store.ListBridgesWithCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list bridges: %v", err))
		return
	}

	passwdFile := os.Getenv("MESHSAT_MOSQUITTO_PASSWD_FILE")
	if passwdFile == "" {
		passwdFile = "/data/mosquitto/passwd"
	}
	aclFile := os.Getenv("MESHSAT_MOSQUITTO_ACL_FILE")
	if aclFile == "" {
		aclFile = "/data/mosquitto/acl"
	}

	passwdData := bridge.GeneratePasswordFile(bridges)
	if err := os.WriteFile(passwdFile, passwdData, 0600); err != nil {
		slog.Error("acl: failed to write password file", "path", passwdFile, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write password file: %v", err))
		return
	}

	aclData := bridge.GenerateACLFile(bridges)
	if err := os.WriteFile(aclFile, aclData, 0600); err != nil {
		slog.Error("acl: failed to write ACL file", "path", aclFile, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write ACL file: %v", err))
		return
	}

	slog.Info("acl: regenerated mosquitto files",
		"bridges", len(bridges), "passwd_file", passwdFile, "acl_file", aclFile)

	writeJSON(w, http.StatusOK, aclRegenResponse{BridgesConfigured: len(bridges)})
}
