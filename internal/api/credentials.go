package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CredentialHandler handles credential management endpoints.
type CredentialHandler struct {
	store     store.Store
	masterKey []byte // AES-256 master key for encrypting credential data
}

// NewCredentialHandler returns a new credential handler.
// masterKey is loaded/bootstrapped from system_config on startup.
func NewCredentialHandler(s store.Store, masterKey []byte) *CredentialHandler {
	return &CredentialHandler{store: s, masterKey: masterKey}
}

// Upload handles ZIP or PEM file upload with x509 parsing.
func (h *CredentialHandler) Upload(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}

	provider := r.FormValue("provider")
	name := r.FormValue("name")
	targetScope := r.FormValue("target_scope")
	targetBridgeID := r.FormValue("target_bridge_id")

	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if name == "" {
		name = provider
	}
	if targetScope == "" {
		targetScope = "hub"
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required: "+err.Error())
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	// Detect ZIP vs PEM
	var pemFiles map[string][]byte
	if isZIP(data) {
		pemFiles, err = extractPEMsFromZIP(data)
		if err != nil {
			writeError(w, http.StatusBadRequest, "extract ZIP: "+err.Error())
			return
		}
	} else {
		pemFiles = map[string][]byte{"upload.pem": data}
	}

	if len(pemFiles) == 0 {
		writeError(w, http.StatusBadRequest, "no PEM files found in upload")
		return
	}

	bundle, err := classifyPEMs(pemFiles)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Encrypt the bundle JSON with master key
	bundleJSON := bundle.toJSON()
	encrypted, err := crypto.Encrypt(h.masterKey, bundleJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encrypt: "+err.Error())
		return
	}

	cred := &store.Credential{
		ID:              uuid.New().String(),
		TenantID:        tid,
		Provider:        provider,
		Name:            name,
		CredType:        bundle.credType(),
		EncryptedData:   encrypted,
		CertSubject:     bundle.subject,
		CertIssuer:      bundle.issuer,
		CertFingerprint: bundle.fingerprint,
		TargetScope:     targetScope,
		TargetBridgeID:  targetBridgeID,
		Status:          "active",
		Version:         1,
	}
	if !bundle.notAfter.IsZero() {
		cred.CertNotAfter = &bundle.notAfter
	}

	if err := h.store.CreateCredential(r.Context(), tid, cred); err != nil {
		writeError(w, http.StatusInternalServerError, "store: "+err.Error())
		return
	}

	slog.Info("credential uploaded", "id", cred.ID, "provider", provider, "name", name,
		"type", cred.CredType, "subject", cred.CertSubject, "tenant", tid)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          cred.ID,
		"provider":    provider,
		"name":        name,
		"cred_type":   cred.CredType,
		"subject":     cred.CertSubject,
		"issuer":      cred.CertIssuer,
		"fingerprint": cred.CertFingerprint,
		"not_after":   cred.CertNotAfter,
	})
}

// List returns all credentials for the tenant (no encrypted data).
func (h *CredentialHandler) List(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	creds, err := h.store.ListCredentials(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Strip encrypted data
	for i := range creds {
		creds[i].EncryptedData = nil
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"credentials": creds})
}

// Get returns a single credential (no encrypted data).
func (h *CredentialHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	cred, err := h.store.GetCredential(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	cred.EncryptedData = nil
	writeJSON(w, http.StatusOK, cred)
}

// Delete removes a credential.
func (h *CredentialHandler) Delete(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteCredential(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	slog.Info("credential deleted", "id", id, "tenant", tid)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ListExpiring returns credentials expiring within N days.
func (h *CredentialHandler) ListExpiring(w http.ResponseWriter, r *http.Request) {
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		fmt.Sscanf(d, "%d", &days)
	}
	before := time.Now().AddDate(0, 0, days)
	creds, err := h.store.ListExpiringCredentials(r.Context(), before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range creds {
		creds[i].EncryptedData = nil
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"credentials": creds,
		"within_days": days,
	})
}

// --- PEM parsing helpers ---

type parsedBundle struct {
	caCertPEM     string
	clientCertPEM string
	clientKeyPEM  string
	subject       string
	issuer        string
	notAfter      time.Time
	fingerprint   string
}

func (b *parsedBundle) credType() string {
	if b.clientCertPEM != "" || b.caCertPEM != "" {
		return "mtls_bundle"
	}
	return "pem_file"
}

func (b *parsedBundle) toJSON() []byte {
	parts := []string{}
	if b.caCertPEM != "" {
		parts = append(parts, fmt.Sprintf(`"ca_cert_pem":%q`, b.caCertPEM))
	}
	if b.clientCertPEM != "" {
		parts = append(parts, fmt.Sprintf(`"client_cert_pem":%q`, b.clientCertPEM))
	}
	if b.clientKeyPEM != "" {
		parts = append(parts, fmt.Sprintf(`"client_key_pem":%q`, b.clientKeyPEM))
	}
	return []byte("{" + strings.Join(parts, ",") + "}")
}

func isZIP(data []byte) bool {
	return len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04
}

func extractPEMsFromZIP(data []byte) (map[string][]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	result := make(map[string][]byte)
	for _, f := range r.File {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".crt") ||
			strings.HasSuffix(name, ".key") || strings.HasSuffix(name, ".cer") {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			content, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				continue
			}
			result[f.Name] = content
		}
	}
	return result, nil
}

func classifyPEMs(pemFiles map[string][]byte) (*parsedBundle, error) {
	bundle := &parsedBundle{}
	for _, data := range pemFiles {
		rest := data
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			switch block.Type {
			case "CERTIFICATE":
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					continue
				}
				if cert.IsCA {
					bundle.caCertPEM = string(pem.EncodeToMemory(block))
					bundle.issuer = cert.Subject.CommonName
				} else {
					bundle.clientCertPEM = string(pem.EncodeToMemory(block))
					bundle.subject = cert.Subject.CommonName
					bundle.notAfter = cert.NotAfter
					fp := sha256.Sum256(cert.Raw)
					bundle.fingerprint = hex.EncodeToString(fp[:])
				}
			case "RSA PRIVATE KEY", "EC PRIVATE KEY", "PRIVATE KEY":
				bundle.clientKeyPEM = string(pem.EncodeToMemory(block))
			}
		}
	}
	if bundle.caCertPEM == "" && bundle.clientCertPEM == "" && bundle.clientKeyPEM == "" {
		return nil, fmt.Errorf("no certificates or keys found in uploaded files")
	}
	return bundle, nil
}
