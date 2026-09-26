package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SupportAccessHandler is the customer's side of support access
// (MESHSAT-1366): a tenant's owner grants the platform a window, with a PIN
// only they know, in which an operator may open their workspace. Without a
// grant a signed-in operator's X-Tenant-ID is refused for this tenant; with
// one, every action the operator takes lands in this tenant's audit log.
type SupportAccessHandler struct {
	store store.Store
	audit *audit.Service
	// Platform bounds on the window, in minutes. Both ends are harmful.
	minMinutes, maxMinutes int
	// forget drops the replica's cached answer for the tenant, so a revoke
	// takes effect on the next request rather than at the end of a TTL.
	forget func(tenantID string)
}

func NewSupportAccessHandler(s store.Store, a *audit.Service, minMinutes, maxMinutes int, forget func(string)) *SupportAccessHandler {
	if minMinutes <= 0 {
		minMinutes = 15
	}
	if maxMinutes < minMinutes {
		maxMinutes = minMinutes
	}
	return &SupportAccessHandler{store: s, audit: a, minMinutes: minMinutes, maxMinutes: maxMinutes, forget: forget}
}

const (
	supportPINMinLen = 10
	supportPINMaxLen = 128
)

type supportAccessResponse struct {
	// Active is whether a grant is in force now: not revoked, not expired.
	Active         bool   `json:"active"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	CreatedByEmail string `json:"created_by_email,omitempty"`
	// UsedAt is when an operator first presented the PIN; empty until then.
	UsedAt      string `json:"used_at,omitempty"`
	UsedByEmail string `json:"used_by_email,omitempty"`
	// The bounds the form offers, so the UI never hardcodes a number that
	// drifts from the ConfigMap.
	MinMinutes int `json:"min_minutes"`
	MaxMinutes int `json:"max_minutes"`
}

func (h *SupportAccessHandler) respond(g *store.SupportGrant) supportAccessResponse {
	resp := supportAccessResponse{MinMinutes: h.minMinutes, MaxMinutes: h.maxMinutes}
	if g == nil {
		return resp
	}
	resp.Active = g.Active(time.Now())
	resp.ExpiresAt = g.ExpiresAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = g.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedByEmail = g.CreatedByEmail
	if g.UsedAt != nil {
		resp.UsedAt = g.UsedAt.UTC().Format(time.RFC3339)
		resp.UsedByEmail = g.UsedByEmail
	}
	return resp
}

// supportAccessTenant is the caller's own tenant, refused for the platform
// tenant, which never needs a grant to itself.
func supportAccessTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tid := hubauth.TenantIDFromContext(r.Context())
	if tid == store.DefaultTenantID {
		writeError(w, http.StatusBadRequest, "the platform tenant does not grant support access to itself")
		return "", false
	}
	return tid, true
}

// Get reports the tenant's current support-access grant.
//
//	@Summary      Support access status
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {object}  supportAccessResponse
//	@Router       /api/tenant/support-access [get]
func (h *SupportAccessHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := supportAccessTenant(w, r)
	if !ok {
		return
	}
	g, err := h.store.GetActiveSupportGrant(r.Context(), tid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "could not read support access")
		return
	}
	writeJSON(w, http.StatusOK, h.respond(g))
}

type supportAccessRequest struct {
	PIN             string `json:"pin"`
	DurationMinutes int    `json:"duration_minutes"`
}

// Create grants the platform support access for a window the owner chooses.
//
//	@Summary      Grant support access
//	@Tags         tenant
//	@Accept       json
//	@Produce      json
//	@Param        body  body      supportAccessRequest  true  "PIN (10 to 128 characters) and window in minutes"
//	@Success      201   {object}  supportAccessResponse
//	@Failure      400   {object}  map[string]string
//	@Router       /api/tenant/support-access [post]
func (h *SupportAccessHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := supportAccessTenant(w, r)
	if !ok {
		return
	}
	var req supportAccessRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pin := strings.TrimSpace(req.PIN)
	if n := utf8.RuneCountInString(pin); n < supportPINMinLen || n > supportPINMaxLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the PIN must be %d to %d characters", supportPINMinLen, supportPINMaxLen))
		return
	}
	if req.DurationMinutes < h.minMinutes || req.DurationMinutes > h.maxMinutes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("duration_minutes must be between %d and %d", h.minMinutes, h.maxMinutes))
		return
	}
	hash, err := hubauth.HashPassword(pin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the PIN")
		return
	}
	u := hubauth.FromContext(r.Context())
	now := time.Now().UTC()
	g := &store.SupportGrant{
		PINHash:   hash,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(req.DurationMinutes) * time.Minute),
	}
	if u != nil {
		g.CreatedByUserID, g.CreatedByEmail = u.ID, u.Email
	}
	if err := h.store.CreateSupportGrant(r.Context(), tid, g); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the grant")
		return
	}
	if h.forget != nil {
		h.forget(tid)
	}
	if h.audit != nil {
		// The PIN is never written anywhere but the hash column.
		detail := fmt.Sprintf("expires_at=%s minutes=%d", g.ExpiresAt.Format(time.RFC3339), req.DurationMinutes)
		if err := h.audit.Log(r.Context(), tid, "support_access_granted", operatorEmail(r), detail, clientIPFromRequest(r)); err != nil {
			writeError(w, http.StatusInternalServerError, "could not record the grant")
			return
		}
	}
	writeJSON(w, http.StatusCreated, h.respond(g))
}

// Revoke ends the current grant early.
//
//	@Summary      Revoke support access
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {object}  supportAccessResponse
//	@Router       /api/tenant/support-access [delete]
func (h *SupportAccessHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	tid, ok := supportAccessTenant(w, r)
	if !ok {
		return
	}
	g, err := h.store.GetActiveSupportGrant(r.Context(), tid)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, h.respond(nil))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read support access")
		return
	}
	if err := h.store.RevokeSupportGrant(r.Context(), tid, g.ID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke the grant")
		return
	}
	if h.forget != nil {
		h.forget(tid)
	}
	if h.audit != nil {
		if err := h.audit.Log(r.Context(), tid, "support_access_revoked", operatorEmail(r), "grant="+g.ID, clientIPFromRequest(r)); err != nil {
			writeError(w, http.StatusInternalServerError, "could not record the revocation")
			return
		}
	}
	writeJSON(w, http.StatusOK, h.respond(nil))
}
