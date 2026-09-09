package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SignupHandler turns beta approval from a script somebody has to remember to
// run into something an operator does in the Hub (MESHSAT-978).
//
// Approval used to have a second half: the address the request came from was
// handed to an n8n workflow, which added it to an allowlist on three VPS
// HAProxy configs, because nothing reached the Hub without being on that list.
// The edge opened at public launch (MESHSAT-995) and the list no longer exists,
// so approval is now what it says it is: activate the account and grant a role.
// The signup address is still recorded, because knowing where a request came
// from is worth having, but it no longer admits anybody to anything.
type SignupHandler struct {
	ak    *authentik.Client
	audit *audit.Service
}

// NewSignupHandler creates the handler. A nil client disables the endpoints.
func NewSignupHandler(ak *authentik.Client, a *audit.Service) *SignupHandler {
	return &SignupHandler{ak: ak, audit: a}
}

func (h *SignupHandler) ready(w http.ResponseWriter) bool {
	if h.ak == nil {
		writeError(w, http.StatusServiceUnavailable, "identity provider not configured")
		return false
	}
	return true
}

// List returns the beta requests waiting for a decision.
//
//	@Summary      List pending beta requests
//	@Tags         admin
//	@Produce      json
//	@Success      200  {object}  map[string]interface{}
//	@Failure      503  {object}  map[string]string
//	@Router       /api/admin/signups [get]
func (h *SignupHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	users, err := h.ak.ListPending(r.Context())
	if err != nil {
		slog.Error("signups: list failed", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the identity provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signups": users, "count": len(users)})
}

type approveRequest struct {
	Role string `json:"role"`
}

// Approve admits a beta request.
//
//	@Summary      Approve a beta request
//	@Tags         admin
//	@Accept       json
//	@Produce      json
//	@Param        id    path      int             true  "authentik user pk"
//	@Param        body  body      approveRequest  true  "role to grant"
//	@Success      200   {object}  map[string]interface{}
//	@Failure      400   {object}  map[string]string
//	@Router       /api/admin/signups/{id}/approve [post]
func (h *SignupHandler) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	pk, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad signup id")
		return
	}
	req := approveRequest{Role: "owner"}
	if r.ContentLength > 0 {
		if err := readJSON(w, r, &req, 1024); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !authentik.ValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, "role must be owner, operator or viewer")
		return
	}
	signupIP, email, err := h.ak.Approve(r.Context(), pk, req.Role)
	if err != nil {
		slog.Error("signups: approve failed", "pk", pk, "error", err)
		writeError(w, http.StatusBadGateway, "approval failed at the identity provider")
		return
	}
	h.log(r, "signup_approved", email, "role="+req.Role+" ip="+signupIP)
	slog.Info("signup approved", "email", email, "role", req.Role, "signup_ip", signupIP)
	writeJSON(w, http.StatusOK, map[string]any{
		"email": email, "role": req.Role, "signup_ip": signupIP,
		"next": "their first sign-in creates their tenant",
	})
}

// Reject deletes a beta request.
//
//	@Summary      Reject a beta request
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      int  true  "authentik user pk"
//	@Success      200  {object}  map[string]string
//	@Failure      400  {object}  map[string]string
//	@Router       /api/admin/signups/{id}/reject [post]
func (h *SignupHandler) Reject(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	pk, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad signup id")
		return
	}
	email, err := h.ak.Reject(r.Context(), pk)
	if err != nil {
		slog.Warn("signups: reject refused", "pk", pk, "error", err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.log(r, "signup_rejected", email, "")
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "status": "rejected"})
}

func (h *SignupHandler) log(r *http.Request, action, subject, detail string) {
	if h.audit == nil {
		return
	}
	actor := "unknown"
	if u := hubauth.FromContext(r.Context()); u != nil && u.Email != "" {
		actor = u.Email
	}
	if detail == "" {
		detail = "email=" + subject
	} else {
		detail = "email=" + subject + " " + detail
	}
	if err := h.audit.Log(r.Context(), store.DefaultTenantID, action, actor, detail, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: failed to log "+action, "error", err)
	}
}
