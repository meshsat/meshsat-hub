package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// supportPINAttempts is how many wrong PINs a grant survives. The sixth
// revokes it and the customer has to grant again: a PIN is at least ten
// characters, so a lockout this early costs an honest operator one phone
// call and an attacker the whole window.
const supportPINAttempts = 5

// SupportGrantCache answers the tenant middleware's question, "may the
// platform open this tenant right now?", from the store with a short cache
// per replica. Forget drops one tenant's answer, which the handlers call on
// every change so an open, a revoke or a lockout applies on the next request
// on THIS replica; the other replica sees it within the TTL, which is why the
// TTL is short.
type SupportGrantCache struct {
	store store.Store
	ttl   time.Duration
	mu    sync.Mutex
	m     map[string]cachedGrant
}

type cachedGrant struct {
	opened bool
	until  time.Time
}

func NewSupportGrantCache(s store.Store, ttl time.Duration) *SupportGrantCache {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &SupportGrantCache{store: s, ttl: ttl, m: map[string]cachedGrant{}}
}

// Opened implements hubauth.SupportGrantLookup.
func (c *SupportGrantCache) Opened(ctx context.Context, tenantID string) (bool, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.m[tenantID]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.opened, nil
	}
	c.mu.Unlock()
	g, err := c.store.GetActiveSupportGrant(ctx, tenantID)
	opened := false
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return false, err
	default:
		opened = g.Active(now) && g.Opened()
	}
	c.mu.Lock()
	c.m[tenantID] = cachedGrant{opened: opened, until: now.Add(c.ttl)}
	c.mu.Unlock()
	return opened, nil
}

// Forget drops the cached answer for one tenant.
func (c *SupportGrantCache) Forget(tenantID string) {
	c.mu.Lock()
	delete(c.m, tenantID)
	c.mu.Unlock()
}

// ViewAsHandler is the operator's side of support access (MESHSAT-1366):
// opening a customer's workspace with the PIN the customer gave, and closing
// it again. Both ends are written to the customer's audit chain and to the
// platform's.
type ViewAsHandler struct {
	store  store.Store
	audit  *audit.Service
	forget func(tenantID string)
}

func NewViewAsHandler(s store.Store, a *audit.Service, forget func(string)) *ViewAsHandler {
	return &ViewAsHandler{store: s, audit: a, forget: forget}
}

type viewAsRequest struct {
	PIN string `json:"pin"`
}

type viewAsResponse struct {
	TenantID  string `json:"tenant_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
}

// viewAsRefusal is the shape every refusal takes, so the console branches on
// the code and the count, never on the words.
type viewAsRefusal struct {
	Error        string `json:"error"`
	Code         string `json:"code"`
	AttemptsLeft *int   `json:"attempts_left,omitempty"`
}

func (h *ViewAsHandler) tenant(w http.ResponseWriter, r *http.Request) (*store.Tenant, bool) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == store.DefaultTenantID {
		writeJSON(w, http.StatusBadRequest, viewAsRefusal{Error: "the platform tenant is your own", Code: "own_tenant"})
		return nil, false
	}
	t, err := h.store.GetTenant(r.Context(), id)
	if err != nil || t == nil {
		writeJSON(w, http.StatusNotFound, viewAsRefusal{Error: "tenant not found", Code: "not_found"})
		return nil, false
	}
	return t, true
}

// Start opens a customer's workspace with the PIN they granted.
//
//	@Summary      Open a tenant as support (needs the customer's PIN)
//	@Tags         admin
//	@Accept       json
//	@Produce      json
//	@Param        id    path      string         true  "Tenant ID"
//	@Param        body  body      viewAsRequest  true  "the PIN the customer granted"
//	@Success      200   {object}  viewAsResponse
//	@Failure      400   {object}  viewAsRefusal  "the platform tenant"
//	@Failure      401   {object}  viewAsRefusal  "wrong PIN, with attempts_left"
//	@Failure      403   {object}  viewAsRefusal  "no grant, or the grant is locked"
//	@Failure      404   {object}  viewAsRefusal
//	@Failure      409   {object}  viewAsRefusal  "the tenant is suspended or closed"
//	@Router       /api/admin/tenants/{id}/view-as [post]
func (h *ViewAsHandler) Start(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req viewAsRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeJSON(w, http.StatusBadRequest, viewAsRefusal{Error: err.Error(), Code: "bad_request"})
		return
	}
	if t.Status != store.TenantActive || t.DeletedAt != nil {
		writeJSON(w, http.StatusConflict, viewAsRefusal{Error: "that tenant is suspended or closed", Code: "not_active"})
		return
	}
	g, err := h.store.GetActiveSupportGrant(r.Context(), t.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusForbidden, viewAsRefusal{Error: "this customer has not granted support access", Code: "support_access_required"})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read support access")
		return
	}
	okPIN, err := hubauth.VerifyPassword(strings.TrimSpace(req.PIN), g.PINHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not check the PIN")
		return
	}
	if !okPIN {
		n, err := h.store.BumpSupportGrantFailures(r.Context(), t.ID, g.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not record the attempt")
			return
		}
		if n >= supportPINAttempts {
			if err := h.store.RevokeSupportGrant(r.Context(), t.ID, g.ID, time.Now().UTC()); err != nil {
				writeError(w, http.StatusInternalServerError, "could not lock the grant")
				return
			}
			if h.forget != nil {
				h.forget(t.ID)
			}
			logPlatformAction(r, h.audit, t.ID, "support_access_locked", "grant="+g.ID+" wrong_pins="+strconv.Itoa(n))
			writeJSON(w, http.StatusForbidden, viewAsRefusal{Error: "too many wrong PINs; the customer has to grant access again", Code: "support_access_locked"})
			return
		}
		left := supportPINAttempts - n
		writeJSON(w, http.StatusUnauthorized, viewAsRefusal{Error: "wrong PIN", Code: "wrong_pin", AttemptsLeft: &left})
		return
	}
	now := time.Now().UTC()
	if !g.Opened() {
		if err := h.store.MarkSupportGrantUsed(r.Context(), t.ID, g.ID, operatorEmail(r), now); err != nil {
			writeError(w, http.StatusInternalServerError, "could not open the grant")
			return
		}
	}
	if h.forget != nil {
		h.forget(t.ID)
	}
	logPlatformAction(r, h.audit, t.ID, "tenant_view_started", "grant="+g.ID+" expires_at="+g.ExpiresAt.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, viewAsResponse{TenantID: t.ID, Slug: t.Slug, Name: t.Name, ExpiresAt: g.ExpiresAt.UTC().Format(time.RFC3339)})
}

// End records that the operator left the customer's workspace. The grant
// keeps running until its expiry or a revoke: the customer chose the window.
//
//	@Summary      Leave a tenant opened as support
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      string  true  "Tenant ID"
//	@Success      200  {object}  map[string]string
//	@Failure      404  {object}  viewAsRefusal
//	@Router       /api/admin/tenants/{id}/view-as [delete]
func (h *ViewAsHandler) End(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	logPlatformAction(r, h.audit, t.ID, "tenant_view_ended", "")
	writeJSON(w, http.StatusOK, map[string]string{"tenant_id": t.ID, "ended_at": now.Format(time.RFC3339)})
}
