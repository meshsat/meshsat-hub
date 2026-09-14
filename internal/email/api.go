package email

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/api"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
)

// APIHandler provides REST endpoints for PGP key management and email testing.
//
// Every handler resolves the CALLER'S tenant and works on that tenant's gateway.
// It used to hold one keyRing and one client for the whole Hub, so a PGP contact
// added by one customer was visible to, and overwritable by, every other -- and
// since Encrypt picks the recipient key by bare address, one tenant could have
// caused another tenant's alert to be encrypted to a key they controlled
// (MESHSAT-1121).
type APIHandler struct {
	pool *Pool
}

// NewAPIHandler creates a new email API handler.
func NewAPIHandler(pool *Pool) *APIHandler {
	return &APIHandler{pool: pool}
}

// gatewayFor resolves the calling tenant's gateway, or writes 503 and returns
// nil. 503 rather than 404: the endpoint exists, this tenant has not configured
// it, and saying so is the whole point of MESHSAT-1121.
func (h *APIHandler) gatewayFor(w http.ResponseWriter, r *http.Request) *Gateway {
	tid := hubauth.TenantIDFromContext(r.Context())
	gw := h.pool.ForTenant(r.Context(), tid)
	if gw == nil {
		api.WriteError(w, http.StatusServiceUnavailable,
			"no email gateway configured for this tenant (Integrations page)")
		return nil
	}
	return gw
}

// GetPublicKey returns the Hub's PGP public key in ASCII-armored format.
//
//	@Summary      Get Hub PGP public key
//	@Tags         email
//	@Produce      text/plain
//	@Success      200  {string}  string
//	@Router       /api/email/keys/public [get]
func (h *APIHandler) GetPublicKey(w http.ResponseWriter, r *http.Request) {
	gw := h.gatewayFor(w, r)
	if gw == nil {
		return
	}
	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Disposition", "attachment; filename=meshsat-hub.asc")
	_, _ = w.Write([]byte(gw.KeyRing.HubPublicKey()))
}

// ListContacts returns metadata for all stored PGP contacts.
//
//	@Summary      List PGP contacts
//	@Tags         email
//	@Produce      json
//	@Success      200  {array}  ContactInfo
//	@Router       /api/email/keys [get]
func (h *APIHandler) ListContacts(w http.ResponseWriter, r *http.Request) {
	gw := h.gatewayFor(w, r)
	if gw == nil {
		return
	}
	infos := gw.KeyRing.ListContactInfo()
	if infos == nil {
		infos = []ContactInfo{}
	}
	api.WriteJSON(w, http.StatusOK, infos)
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
	var req addContactRequest
	if err := api.ReadJSON(w, r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Email == "" || req.ArmoredKey == "" {
		api.WriteError(w, http.StatusBadRequest, "email and armored_key required")
		return
	}

	gw := h.gatewayFor(w, r)
	if gw == nil {
		return
	}
	// Parse FIRST, so an unusable key is refused before it is stored. Persisting
	// a key that cannot be read would be worse than refusing it: the load path
	// skips it and the recipient silently falls back to cleartext.
	if err := gw.KeyRing.AddContact(req.Email, req.ArmoredKey); err != nil {
		slog.Error("email: add contact key failed", "email", req.Email, "error", err)
		api.WriteError(w, http.StatusBadRequest, "invalid PGP key")
		return
	}

	tid := hubauth.TenantIDFromContext(r.Context())
	if err := h.pool.PersistContact(r.Context(), tid, req.Email, req.ArmoredKey); err != nil {
		slog.Error("email: storing the contact key failed", "tenant", tid, "email", req.Email, "error", err)
		api.WriteError(w, http.StatusInternalServerError, "could not store the key")
		return
	}

	slog.Info("email: contact key added", "tenant", tid, "email", req.Email)
	api.WriteJSON(w, http.StatusCreated, map[string]string{"status": "ok", "email": req.Email})
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
		api.WriteError(w, http.StatusBadRequest, "missing email")
		return
	}
	gw := h.gatewayFor(w, r)
	if gw == nil {
		return
	}
	gw.KeyRing.RemoveContact(email)
	tid := hubauth.TenantIDFromContext(r.Context())
	if err := h.pool.ForgetContact(r.Context(), tid, email); err != nil {
		slog.Error("email: removing the stored contact key failed", "tenant", tid, "email", email, "error", err)
		api.WriteError(w, http.StatusInternalServerError, "could not remove the key")
		return
	}
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
	var req testEmailRequest
	if err := api.ReadJSON(w, r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.To == "" {
		api.WriteError(w, http.StatusBadRequest, "to is required")
		return
	}

	gw := h.gatewayFor(w, r)
	if gw == nil {
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

	if err := gw.Client.Send(req.To, subject, msgBody); err != nil {
		api.WriteError(w, http.StatusInternalServerError, "send failed: "+err.Error())
		return
	}

	encrypted := gw.KeyRing.GetContact(req.To) != nil
	api.WriteJSON(w, http.StatusOK, map[string]interface{}{
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
