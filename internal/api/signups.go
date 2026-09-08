package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SignupHandler turns beta approval from a script somebody has to remember to
// run into something an operator does in the Hub (MESHSAT-978).
//
// The Hub does not admit anyone to the edge itself: that means SSH to three
// VPS in three countries, which is not a capability a public web service
// should have. Approval posts to the n8n workflow already wired for signups,
// and n8n runs the allowlist script. If that hop fails the approval still
// stands, and the address is in the response and the log, because an operator
// who has approved someone needs to know the account is live either way.
type SignupHandler struct {
	ak         *authentik.Client
	audit      *audit.Service
	webhookURL string
	http       *http.Client
}

// NewSignupHandler creates the handler. A nil client disables the endpoints.
func NewSignupHandler(ak *authentik.Client, a *audit.Service, webhookURL string) *SignupHandler {
	return &SignupHandler{ak: ak, audit: a, webhookURL: webhookURL, http: &http.Client{Timeout: 15 * time.Second}}
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
	admitted := h.admitAtEdge(r.Context(), signupIP, email)
	h.log(r, "signup_approved", email, "role="+req.Role+" ip="+signupIP+" edge="+admitted)
	slog.Info("signup approved", "email", email, "role", req.Role, "signup_ip", signupIP, "edge", admitted)
	writeJSON(w, http.StatusOK, map[string]any{
		"email": email, "role": req.Role, "signup_ip": signupIP, "edge_allowlist": admitted,
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

// admitAtEdge asks n8n to add the address to the three VPS allowlists and
// reports what happened, in words an operator can act on.
func (h *SignupHandler) admitAtEdge(ctx context.Context, ip, email string) string {
	switch {
	case ip == "":
		return "no address recorded at signup; add it by hand once they tell you"
	case h.webhookURL == "":
		return "not requested: no workflow configured; run k8s/scripts/edge/whitelist-ip.sh " + ip
	}
	body, _ := json.Marshal(map[string]string{"action": "whitelist", "ip": ip, "email": email})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.webhookURL, bytes.NewReader(body))
	if err != nil {
		return "request could not be built; run k8s/scripts/edge/whitelist-ip.sh " + ip
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		slog.Warn("signups: edge allowlist request failed", "ip", ip, "error", err)
		return "failed; run k8s/scripts/edge/whitelist-ip.sh " + ip
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		slog.Warn("signups: edge allowlist refused", "ip", ip, "status", resp.Status)
		return "refused (" + resp.Status + "); run k8s/scripts/edge/whitelist-ip.sh " + ip
	}
	return "requested"
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
