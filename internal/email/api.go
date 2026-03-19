package email

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// suppress unused import warning
var _ = slog.Info

// APIHandler provides REST endpoints for PGP key management and email testing.
type APIHandler struct {
	keyRing *KeyRing
	client  *Client
}

// NewAPIHandler creates a new email API handler.
func NewAPIHandler(kr *KeyRing) *APIHandler {
	return &APIHandler{keyRing: kr}
}

// SetClient sets the SMTP client for test send functionality.
func (h *APIHandler) SetClient(c *Client) {
	h.client = c
}

// GetPublicKey returns the Hub's PGP public key in ASCII-armored format.
//
//	@Summary      Get Hub PGP public key
//	@Tags         email
//	@Produce      text/plain
//	@Success      200  {string}  string
//	@Router       /api/email/keys/public [get]
func (h *APIHandler) GetPublicKey(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Disposition", "attachment; filename=meshsat-hub.asc")
	_, _ = w.Write([]byte(h.keyRing.HubPublicKey()))
}

// ListContacts returns metadata for all stored PGP contacts.
//
//	@Summary      List PGP contacts
//	@Tags         email
//	@Produce      json
//	@Success      200  {array}  ContactInfo
//	@Router       /api/email/keys [get]
func (h *APIHandler) ListContacts(w http.ResponseWriter, _ *http.Request) {
	infos := h.keyRing.ListContactInfo()
	if infos == nil {
		infos = []ContactInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(infos)
}

type addContactRequest struct {
	Email      string `json:"email"`
	ArmoredKey string `json:"armored_key"`
}

// AddContact stores a recipient's PGP public key.
//
//	@Summary      Add PGP contact key
//	@Tags         email
//	@Accept       json
//	@Produce      json
//	@Param        body  body  addContactRequest  true  "Contact key"
//	@Success      201  {object}  map[string]string
//	@Failure      400  {object}  map[string]string
//	@Router       /api/email/keys [post]
func (h *APIHandler) AddContact(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
		return
	}

	var req addContactRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.ArmoredKey == "" {
		http.Error(w, `{"error":"email and armored_key required"}`, http.StatusBadRequest)
		return
	}

	if err := h.keyRing.AddContact(req.Email, req.ArmoredKey); err != nil {
		slog.Error("email: add contact key failed", "email", req.Email, "error", err)
		http.Error(w, `{"error":"invalid PGP key"}`, http.StatusBadRequest)
		return
	}

	slog.Info("email: contact key added", "email", req.Email)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "email": req.Email})
}

// DeleteContact removes a recipient's PGP public key.
//
//	@Summary      Delete PGP contact key
//	@Tags         email
//	@Param        email  path  string  true  "Contact email"
//	@Success      204
//	@Router       /api/email/keys/{email} [delete]
func (h *APIHandler) DeleteContact(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	if email == "" {
		http.Error(w, `{"error":"missing email"}`, http.StatusBadRequest)
		return
	}
	h.keyRing.RemoveContact(email)
	w.WriteHeader(http.StatusNoContent)
}

// TestSend sends a test email to verify the gateway is working.
//
//	@Summary      Send test email
//	@Tags         email
//	@Accept       json
//	@Produce      json
//	@Param        body  body  testEmailRequest  true  "Test email parameters"
//	@Success      200  {object}  map[string]string
//	@Failure      400  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/email/test [post]
func (h *APIHandler) TestSend(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
		return
	}

	var req testEmailRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	if req.To == "" {
		http.Error(w, `{"error":"to is required"}`, http.StatusBadRequest)
		return
	}

	if h.client == nil {
		http.Error(w, `{"error":"email gateway not configured"}`, http.StatusServiceUnavailable)
		return
	}

	subject := "MeshSat Hub — Test Email"
	if req.Subject != "" {
		subject = req.Subject
	}
	msgBody := "This is a test email from MeshSat Hub.\n\nIf this email is PGP-encrypted, your key is correctly configured."
	if req.Body != "" {
		msgBody = req.Body
	}

	if err := h.client.Send(req.To, subject, msgBody); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"send failed: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	encrypted := h.keyRing.GetContact(req.To) != nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "sent",
		"to":        req.To,
		"encrypted": encrypted,
	})
}

type testEmailRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}
