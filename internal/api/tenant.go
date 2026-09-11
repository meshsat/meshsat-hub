package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// inviteTTL is how long a tenant invite stays valid.
const inviteTTL = 14 * 24 * time.Hour

// TenantHandler serves the signed-in tenant (/api/tenant) and the
// platform-admin view of all tenants (/api/admin/tenants).
type TenantHandler struct {
	store store.Store
	// forget drops the cached lifecycle status across the replicas, so a
	// suspension applies to the next request rather than at the end of a TTL.
	forget func(tenantID string)
}

// NewTenantHandler creates the tenant handler.
func NewTenantHandler(s store.Store) *TenantHandler {
	return &TenantHandler{store: s}
}

// SetStatusInvalidator wires the cross-replica cache drop.
func (h *TenantHandler) SetStatusInvalidator(f func(tenantID string)) { h.forget = f }

type tenantResponse struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id,omitempty"`
	Plan        string `json:"plan"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// PurgeGraceDays is how long a closed account can still be recovered. The
	// close dialog quotes it to the customer, and a number the UI hardcoded
	// would be one refactor away from disagreeing with what actually happens.
	PurgeGraceDays int `json:"purge_grace_days"`
}

func toTenantResponse(t *store.Tenant) tenantResponse {
	return tenantResponse{
		ID: t.ID, Slug: t.Slug, Name: t.Name, OwnerUserID: t.OwnerUserID, Plan: t.Plan, Status: t.Status,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
		PurgeGraceDays: int(store.PurgeGrace / (24 * time.Hour)),
	}
}

type inviteResponse struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	ExpiresAt  string `json:"expires_at"`
	AcceptedAt string `json:"accepted_at,omitempty"`
	CreatedAt  string `json:"created_at"`
}

func toInviteResponse(i *store.TenantInvite) inviteResponse {
	r := inviteResponse{ID: i.ID, Email: i.Email, Role: i.Role, ExpiresAt: i.ExpiresAt.UTC().Format(time.RFC3339), CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339)}
	if !i.AcceptedAt.IsZero() {
		r.AcceptedAt = i.AcceptedAt.UTC().Format(time.RFC3339)
	}
	return r
}

type updateTenantRequest struct {
	Name string `json:"name"`
}

type createInviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type adminUpdateTenantRequest struct {
	Name   string `json:"name,omitempty"`
	Plan   string `json:"plan,omitempty"`
	Status string `json:"status,omitempty"`
	// PlanExpiresAt sets or clears when the plan lapses back to free. A
	// pointer, so the three cases stay distinguishable: absent leaves it
	// alone, "" clears it, and a date sets it. Nothing anywhere could write
	// this field before, so an operator-set plan on a tenant that still
	// carried an expiry lapsed back to free on its own, and fixing it
	// meant going into the database by hand (MESHSAT-989).
	PlanExpiresAt *string `json:"plan_expires_at,omitempty"`
}

// Get returns the signed-in user's tenant.
// @Summary      Current tenant
// @Tags         tenant
// @Produce      json
// @Success      200  {object}  tenantResponse
// @Failure      404  {object}  map[string]string
// @Router       /api/tenant [get]
func (h *TenantHandler) Get(w http.ResponseWriter, r *http.Request) {
	t, err := h.store.GetTenant(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	writeJSON(w, http.StatusOK, toTenantResponse(t))
}

// Update renames the signed-in user's tenant (owner-only).
// @Summary      Rename current tenant
// @Tags         tenant
// @Accept       json
// @Produce      json
// @Param        body  body      updateTenantRequest  true  "New name"
// @Success      200   {object}  tenantResponse
// @Failure      400   {object}  map[string]string
// @Router       /api/tenant [put]
func (h *TenantHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req updateTenantRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 {
		writeError(w, http.StatusBadRequest, "name must be 1-80 characters")
		return
	}
	t, err := h.store.GetTenant(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	t.Name = name
	if err := h.store.UpdateTenant(r.Context(), t); err != nil {
		slog.Error("tenant: update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, toTenantResponse(t))
}

// ListInvites lists the tenant's invites (owner-only).
// @Summary      List tenant invites
// @Tags         tenant
// @Produce      json
// @Success      200  {array}  inviteResponse
// @Router       /api/tenant/invites [get]
func (h *TenantHandler) ListInvites(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListInvites(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]inviteResponse, 0, len(items))
	for i := range items {
		out = append(out, toInviteResponse(&items[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateInvite invites an email address into the tenant (owner-only). The
// invitee signs in with their MeshSat ID; a verified email that matches a
// pending invite joins this tenant with the given role.
// @Summary      Invite a user into the tenant
// @Tags         tenant
// @Accept       json
// @Produce      json
// @Param        body  body      createInviteRequest  true  "Invite"
// @Success      201   {object}  inviteResponse
// @Failure      400   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/tenant/invites [post]
func (h *TenantHandler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	var req createInviteRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		writeError(w, http.StatusBadRequest, "valid email required")
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if role == "" {
		role = hubauth.RoleOperator
	}
	if role != hubauth.RoleViewer && role != hubauth.RoleOperator && role != hubauth.RoleOwner {
		writeError(w, http.StatusBadRequest, "role must be viewer, operator or owner")
		return
	}
	tenantID := hubauth.TenantIDFromContext(r.Context())
	if u, _ := h.store.GetUserByEmail(r.Context(), tenantID, email); u != nil {
		writeError(w, http.StatusConflict, "already a member of this tenant")
		return
	}
	if p, _ := h.store.GetPendingInviteByEmail(r.Context(), email); p != nil {
		writeError(w, http.StatusConflict, "a pending invite already exists for this email")
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	sum := sha256.Sum256(raw)
	inv := &store.TenantInvite{Email: email, Role: role, TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: time.Now().UTC().Add(inviteTTL)}
	if err := h.store.CreateInvite(r.Context(), tenantID, inv); err != nil {
		slog.Error("tenant: create invite failed", "error", err)
		writeError(w, http.StatusInternalServerError, "invite failed")
		return
	}
	slog.Info("tenant: invite created", "tenant", tenantID, "email", email, "role", role)
	writeJSON(w, http.StatusCreated, toInviteResponse(inv))
}

// DeleteInvite revokes an invite (owner-only).
// @Summary      Revoke a tenant invite
// @Tags         tenant
// @Param        id  path  string  true  "Invite ID"
// @Success      204
// @Router       /api/tenant/invites/{id} [delete]
func (h *TenantHandler) DeleteInvite(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" || len(id) > 64 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.store.DeleteInvite(r.Context(), hubauth.TenantIDFromContext(r.Context()), id); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminList lists every tenant (platform administrators only).
// @Summary      List all tenants
// @Tags         admin
// @Produce      json
// @Success      200  {array}  tenantResponse
// @Router       /api/admin/tenants [get]
func (h *TenantHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListTenants(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]tenantResponse, 0, len(items))
	for i := range items {
		out = append(out, toTenantResponse(&items[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminUpdate changes a tenant's name, plan or status (platform administrators only).
// @Summary      Update a tenant
// @Tags         admin
// @Accept       json
// @Produce      json
// @Param        id    path      string                    true  "Tenant ID"
// @Param        body  body      adminUpdateTenantRequest  true  "Fields to change"
// @Success      200   {object}  tenantResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/admin/tenants/{id} [put]
func (h *TenantHandler) AdminUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := h.store.GetTenant(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	var req adminUpdateTenantRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if v := strings.TrimSpace(req.Name); v != "" {
		if len(v) > 80 {
			writeError(w, http.StatusBadRequest, "name must be 1-80 characters")
			return
		}
		t.Name = v
	}
	if v := strings.ToLower(strings.TrimSpace(req.Plan)); v != "" {
		// A plan name now decides a device ceiling, so it has to be one of the
		// tiers rather than any short string. plans.For falls back to the free
		// limits for a name it does not know, and a typo here would otherwise
		// cap a paying tenant at four devices.
		if !plans.Known(v) {
			writeError(w, http.StatusBadRequest, "plan must be one of: "+strings.Join(plans.Names(), ", "))
			return
		}
		t.Plan = plans.Normalise(v)
	}
	if v := strings.ToLower(strings.TrimSpace(req.Status)); v != "" {
		if v != "active" && v != "suspended" {
			writeError(w, http.StatusBadRequest, "status must be active or suspended")
			return
		}
		if t.ID == store.DefaultTenantID && v == "suspended" {
			writeError(w, http.StatusBadRequest, "the default tenant cannot be suspended")
			return
		}
		t.Status = v
	}
	if req.PlanExpiresAt != nil {
		switch v := strings.TrimSpace(*req.PlanExpiresAt); v {
		case "":
			// An operator-set plan is meant to be permanent: no expiry, so the
			// lapse job never looks at it again.
			t.PlanExpiresAt = nil
		default:
			when, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "plan_expires_at must be RFC3339, or \"\" to clear it")
				return
			}
			when = when.UTC()
			t.PlanExpiresAt = &when
		}
	}
	if err := h.store.UpdateTenant(r.Context(), t); err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if h.forget != nil {
		h.forget(t.ID)
	}
	admin := hubauth.FromContext(r.Context())
	slog.Info("tenant: updated by platform admin", "tenant", t.ID, "by", admin.ID, "status", t.Status, "plan", t.Plan)
	writeJSON(w, http.StatusOK, toTenantResponse(t))
}

// validEmail is a conservative shape check; the IdP verifies ownership.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at < 1 || at == len(s)-1 || strings.ContainsAny(s, " \t\r\n<>,;\"") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}
