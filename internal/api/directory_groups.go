package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/directory"
)

// Group and dispatch-policy REST, tenant-scoped like the contacts: the
// tenant always comes from the caller, never from the body. Writes need
// the Operator role (router). Each write bumps the tenant's directory
// version in its own transaction. [MESHSAT-1584 C7]

// ─── Groups ──────────────────────────────────────────────────────────

// ListGroups returns every group of the caller's tenant.
// @Summary List directory groups
// @Description Every group of the caller's tenant with its members in their order.
// @Tags directory
// @Produce json
// @Success 200 {array} directory.Group
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/groups [get]
func (h *DirectoryHandler) ListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.store.ListGroups(r.Context(), auth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if groups == nil {
		groups = []directory.Group{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// GetGroup returns one group by ID.
// @Summary Get directory group
// @Tags directory
// @Produce json
// @Param id path string true "Group ID"
// @Success 200 {object} directory.Group
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/groups/{id} [get]
func (h *DirectoryHandler) GetGroup(w http.ResponseWriter, r *http.Request) {
	g, err := h.store.GetGroup(r.Context(), auth.TenantIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// CreateGroup stores a new group in the caller's tenant. The body may omit
// the ID (the Hub makes one); a supplied ID is kept.
// @Summary Create directory group
// @Description kind is TEAM, ROLE, LIST (the default) or MLS_GROUP. member_ids name contacts of the caller's tenant, in order; a repeated id counts once.
// @Tags directory
// @Accept json
// @Produce json
// @Param body body directory.Group true "Group (tenant_id is the caller's)"
// @Success 201 {object} directory.Group
// @Failure 400 {object} map[string]string "Missing name or unknown kind"
// @Failure 409 {object} map[string]string "A member is not a contact of the tenant"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/groups [post]
func (h *DirectoryHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var body directory.Group
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	body.TenantID = auth.TenantIDFromContext(r.Context())
	body.CreatedAt, body.UpdatedAt = time.Time{}, time.Time{}
	if err := h.store.PutGroup(r.Context(), &body); err != nil {
		writeDirectoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// UpdateGroup replaces a group of the caller's tenant by ID.
// @Summary Update directory group
// @Description Replaces the group's fields and its member list.
// @Tags directory
// @Accept json
// @Produce json
// @Param id path string true "Group ID"
// @Param body body directory.Group true "Group"
// @Success 200 {object} directory.Group
// @Failure 400 {object} map[string]string "Missing name or unknown kind"
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string "A member is not a contact of the tenant"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/groups/{id} [put]
func (h *DirectoryHandler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	existing, err := h.store.GetGroup(r.Context(), tid, id)
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	var body directory.Group
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	body.ID, body.TenantID = id, tid
	body.CreatedAt, body.UpdatedAt = existing.CreatedAt, time.Time{}
	if err := h.store.PutGroup(r.Context(), &body); err != nil {
		writeDirectoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// DeleteGroup removes a group of the caller's tenant and its memberships.
// @Summary Delete directory group
// @Tags directory
// @Param id path string true "Group ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/groups/{id} [delete]
func (h *DirectoryHandler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	err := h.store.DeleteGroup(r.Context(), auth.TenantIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, directory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Dispatch policies ───────────────────────────────────────────────

// ListPolicies returns every dispatch policy of the caller's tenant.
// @Summary List directory dispatch policies
// @Description Every dispatch policy of the caller's tenant, the seeded precedence defaults included.
// @Tags directory
// @Produce json
// @Success 200 {array} directory.DispatchPolicy
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/policies [get]
func (h *DirectoryHandler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.store.ListPolicies(r.Context(), auth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if policies == nil {
		policies = []directory.DispatchPolicy{}
	}
	writeJSON(w, http.StatusOK, policies)
}

// GetPolicy returns one dispatch policy by ID.
// @Summary Get directory dispatch policy
// @Tags directory
// @Produce json
// @Param id path string true "Policy ID"
// @Success 200 {object} directory.DispatchPolicy
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/policies/{id} [get]
func (h *DirectoryHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetPolicy(r.Context(), auth.TenantIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// CreatePolicy stores a new dispatch policy in the caller's tenant.
// @Summary Create directory dispatch policy
// @Description scope_type is contact or group (scope_id names it), precedence (scope_id is Override, Flash, Immediate, Priority, Routine or Deferred) or default (no scope_id). strategy is PRIMARY_ONLY, ANY_REACHABLE, ORDERED_FALLBACK, HEMB_BONDED or ALL_BEARERS. One policy per scope.
// @Tags directory
// @Accept json
// @Produce json
// @Param body body directory.DispatchPolicy true "Policy"
// @Success 201 {object} directory.DispatchPolicy
// @Failure 400 {object} map[string]string "Unknown scope, precedence, strategy or bearer kind"
// @Failure 409 {object} map[string]string "Another policy covers this scope"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/policies [post]
func (h *DirectoryHandler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var body directory.DispatchPolicy
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.TenantID = auth.TenantIDFromContext(r.Context())
	body.CreatedAt, body.UpdatedAt = time.Time{}, time.Time{}
	if err := h.store.PutPolicy(r.Context(), &body); err != nil {
		writeDirectoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// UpdatePolicy replaces a dispatch policy of the caller's tenant by ID. A
// seeded default the operator writes is the operator's from then on.
// @Summary Update directory dispatch policy
// @Tags directory
// @Accept json
// @Produce json
// @Param id path string true "Policy ID"
// @Param body body directory.DispatchPolicy true "Policy"
// @Success 200 {object} directory.DispatchPolicy
// @Failure 400 {object} map[string]string "Unknown scope, precedence, strategy or bearer kind"
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string "Another policy covers this scope"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/policies/{id} [put]
func (h *DirectoryHandler) UpdatePolicy(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	existing, err := h.store.GetPolicy(r.Context(), tid, id)
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	var body directory.DispatchPolicy
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.ID, body.TenantID = id, tid
	body.CreatedAt, body.UpdatedAt = existing.CreatedAt, time.Time{}
	if err := h.store.PutPolicy(r.Context(), &body); err != nil {
		writeDirectoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// DeletePolicy removes a dispatch policy of the caller's tenant.
// @Summary Delete directory dispatch policy
// @Tags directory
// @Param id path string true "Policy ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/policies/{id} [delete]
func (h *DirectoryHandler) DeletePolicy(w http.ResponseWriter, r *http.Request) {
	err := h.store.DeletePolicy(r.Context(), auth.TenantIDFromContext(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, directory.ErrNotFound) {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
