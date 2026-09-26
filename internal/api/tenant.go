package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"log/slog"
	"net/http"
	"strconv"
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
	// audit records every operator action on a tenant, on the tenant's own
	// chain and mirrored to the platform's (MESHSAT-1366).
	audit *audit.Service
	// forget drops the cached lifecycle status across the replicas, so a
	// suspension applies to the next request rather than at the end of a TTL.
	forget func(tenantID string)
	// The platform's bridge-offline-timeout policy: the default every tenant
	// gets, and the bounds an owner may choose within (MESHSAT-1117).
	bridgeTimeoutDefault int
	bridgeTimeoutMin     int
	bridgeTimeoutMax     int
	// The platform's audit-retention policy, same shape.
	auditRetentionDefault int
	auditRetentionMin     int
	auditRetentionMax     int
	// The per-device send budget a tenant owner may set: anything from 1 to
	// max. The default is what applies when they set nothing; max is a sanity
	// ceiling. Monthly default 0 means the platform sets no monthly limit.
	sendCapDailyDefault, sendCapDailyMax     int
	sendCapMonthlyDefault, sendCapMonthlyMax int
	// The platform's out-of-band command policy, same shape (MESHSAT-1121).
	oobMaxPerHourDefault int
	oobMaxPerHourMin     int
	oobMaxPerHourMax     int
	oobSMSTimeoutDefault int
	oobSatTimeoutDefault int
	oobTimeoutMin        int
	oobTimeoutMax        int
}

// NewTenantHandler creates the tenant handler.
func NewTenantHandler(s store.Store) *TenantHandler {
	return &TenantHandler{store: s}
}

// SetAudit gives the handler the chain every operator action is written to.
func (h *TenantHandler) SetAudit(a *audit.Service) { h.audit = a }

// SetBridgeOfflineTimeoutPolicy gives the handler the platform's default and
// the bounds a tenant owner may choose within (MESHSAT-1117). Without it the
// setting is not offered at all, which is what a Hub built before this had.
func (h *TenantHandler) SetBridgeOfflineTimeoutPolicy(def, min, max int) {
	h.bridgeTimeoutDefault, h.bridgeTimeoutMin, h.bridgeTimeoutMax = def, min, max
}

// SetAuditRetentionPolicy gives the handler the platform's default retention
// and the bounds a tenant owner may choose within (MESHSAT-1117).
// SetSendCapPolicy gives the handler the platform's per-device send budget
// (what applies when a tenant sets nothing) and the ceilings.
func (h *TenantHandler) SetSendCapPolicy(dailyDefault, dailyMax, monthlyDefault, monthlyMax int) {
	h.sendCapDailyDefault, h.sendCapDailyMax = dailyDefault, dailyMax
	h.sendCapMonthlyDefault, h.sendCapMonthlyMax = monthlyDefault, monthlyMax
}

func (h *TenantHandler) SetAuditRetentionPolicy(def, min, max int) {
	h.auditRetentionDefault, h.auditRetentionMin, h.auditRetentionMax = def, min, max
}

// SetOOBPolicy gives the handler the platform's out-of-band defaults and the
// bounds a tenant owner may choose within (MESHSAT-1121). Seconds throughout, so
// the wire format needs no duration parsing.
func (h *TenantHandler) SetOOBPolicy(maxPerHour, maxMin, maxMax, smsDefault, satDefault, timeoutMin, timeoutMax int) {
	h.oobMaxPerHourDefault, h.oobMaxPerHourMin, h.oobMaxPerHourMax = maxPerHour, maxMin, maxMax
	h.oobSMSTimeoutDefault, h.oobSatTimeoutDefault = smsDefault, satDefault
	h.oobTimeoutMin, h.oobTimeoutMax = timeoutMin, timeoutMax
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
	// BridgeOfflineTimeout is the tenant's own choice in seconds, or 0 meaning
	// "use the platform default" -- which BridgeOfflineTimeoutDefault carries,
	// so the UI can show the number that is actually in force instead of
	// hardcoding one that can drift from the ConfigMap.
	BridgeOfflineTimeout        int `json:"bridge_offline_timeout"`
	BridgeOfflineTimeoutDefault int `json:"bridge_offline_timeout_default"`
	BridgeOfflineTimeoutMin     int `json:"bridge_offline_timeout_min"`
	BridgeOfflineTimeoutMax     int `json:"bridge_offline_timeout_max"`
	// AuditRetentionDays is the tenant's own choice in days, 0 meaning the
	// platform default, which AuditRetentionDefault carries.
	AuditRetentionDays    int `json:"audit_retention_days"`
	AuditRetentionDefault int `json:"audit_retention_default"`
	AuditRetentionMin     int `json:"audit_retention_min"`
	AuditRetentionMax     int `json:"audit_retention_max"`
	// The per-device send budget: how many messages the Hub will send to ONE
	// of this tenant's devices per UTC day and per month (SOS is never
	// counted). 0 is "the platform default". The airtime is the tenant's own
	// carrier account, so the owner sets this, above or below the default; a
	// monthly default of 0 means no monthly limit.
	RatelimitDailyCap       int `json:"ratelimit_daily_cap"`
	RatelimitDailyDefault   int `json:"ratelimit_daily_default"`
	RatelimitDailyMax       int `json:"ratelimit_daily_max"`
	RatelimitMonthlyCap     int `json:"ratelimit_monthly_cap"`
	RatelimitMonthlyDefault int `json:"ratelimit_monthly_default"`
	RatelimitMonthlyMax     int `json:"ratelimit_monthly_max"`
	// Out-of-band command policy: this tenant's own choices, 0 meaning the
	// platform default, with that default and the bounds alongside so the form
	// shows the number actually in force (MESHSAT-1121).
	OOBMaxPerHour        int `json:"oob_max_per_hour"`
	OOBMaxPerHourDefault int `json:"oob_max_per_hour_default"`
	OOBMaxPerHourMin     int `json:"oob_max_per_hour_min"`
	OOBMaxPerHourMax     int `json:"oob_max_per_hour_max"`
	OOBSMSTimeoutSec     int `json:"oob_sms_timeout_sec"`
	OOBSMSTimeoutDefault int `json:"oob_sms_timeout_default"`
	OOBSatTimeoutSec     int `json:"oob_sat_timeout_sec"`
	OOBSatTimeoutDefault int `json:"oob_sat_timeout_default"`
	OOBTimeoutMin        int `json:"oob_timeout_min"`
	OOBTimeoutMax        int `json:"oob_timeout_max"`
}

func (h *TenantHandler) toTenantResponse(t *store.Tenant) tenantResponse {
	r := toTenantResponse(t)
	r.BridgeOfflineTimeoutDefault = h.bridgeTimeoutDefault
	r.BridgeOfflineTimeoutMin = h.bridgeTimeoutMin
	r.BridgeOfflineTimeoutMax = h.bridgeTimeoutMax
	r.AuditRetentionDefault = h.auditRetentionDefault
	r.AuditRetentionMin = h.auditRetentionMin
	r.AuditRetentionMax = h.auditRetentionMax
	r.OOBMaxPerHourDefault = h.oobMaxPerHourDefault
	r.OOBMaxPerHourMin = h.oobMaxPerHourMin
	r.OOBMaxPerHourMax = h.oobMaxPerHourMax
	r.OOBSMSTimeoutDefault = h.oobSMSTimeoutDefault
	r.OOBSatTimeoutDefault = h.oobSatTimeoutDefault
	r.OOBTimeoutMin = h.oobTimeoutMin
	r.OOBTimeoutMax = h.oobTimeoutMax
	r.RatelimitDailyDefault = h.sendCapDailyDefault
	r.RatelimitDailyMax = h.sendCapDailyMax
	r.RatelimitMonthlyDefault = h.sendCapMonthlyDefault
	r.RatelimitMonthlyMax = h.sendCapMonthlyMax
	return r
}

func toTenantResponse(t *store.Tenant) tenantResponse {
	return tenantResponse{
		ID: t.ID, Slug: t.Slug, Name: t.Name, OwnerUserID: t.OwnerUserID, Plan: t.Plan, Status: t.Status,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
		PurgeGraceDays:       int(store.PurgeGrace / (24 * time.Hour)),
		BridgeOfflineTimeout: t.BridgeOfflineTimeout,
		AuditRetentionDays:   t.AuditRetentionDays,
		RatelimitDailyCap:    t.RatelimitDailyCap,
		RatelimitMonthlyCap:  t.RatelimitMonthlyCap,
		OOBMaxPerHour:        t.OOBMaxPerHour,
		OOBSMSTimeoutSec:     t.OOBSMSTimeoutSec,
		OOBSatTimeoutSec:     t.OOBSatTimeoutSec,
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
	// BridgeOfflineTimeout is a pointer so the three cases stay
	// distinguishable: absent leaves the setting alone, 0 returns the tenant to
	// the platform default, and a number sets it. A plain int would make every
	// name change silently reset the timeout to the default.
	BridgeOfflineTimeout *int `json:"bridge_offline_timeout,omitempty"`
	// AuditRetentionDays, same pointer semantics: absent leaves it alone,
	// 0 returns the tenant to the platform default.
	AuditRetentionDays *int `json:"audit_retention_days,omitempty"`
	// RatelimitDailyCap and RatelimitMonthlyCap are this tenant's per-device
	// send budget, set by its owner (owner ruling, 21 Sep 2026: every tenant
	// pays its own carrier for these messages, so the limit on them is the
	// tenant's to choose). Same pointer semantics: absent leaves it alone, 0
	// returns to the platform default.
	RatelimitDailyCap   *int `json:"ratelimit_daily_cap,omitempty"`
	RatelimitMonthlyCap *int `json:"ratelimit_monthly_cap,omitempty"`
	// Out-of-band command policy, same pointer semantics throughout: absent
	// leaves it alone, 0 returns the tenant to the platform default. Owner
	// editable, unlike the send caps -- these are the tenant's OWN kit on the
	// tenant's OWN airtime, not a commercial lever (MESHSAT-1121).
	OOBMaxPerHour    *int `json:"oob_max_per_hour,omitempty"`
	OOBSMSTimeoutSec *int `json:"oob_sms_timeout_sec,omitempty"`
	OOBSatTimeoutSec *int `json:"oob_sat_timeout_sec,omitempty"`
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
	// RatelimitDailyCap and RatelimitMonthlyCap set this tenant's per-device
	// send budget (MESHSAT-1117 tranche 2c). Until 21 Sep 2026 this was the
	// only way to set it, on the reasoning that a send budget is a commercial
	// lever. It is not: the meter is devices because the airtime is the
	// tenant's own, so the owner now sets it in Settings, and this stays for
	// an operator helping a customer. No upper bound here.
	//
	// Pointers, like PlanExpiresAt: absent leaves it alone, 0 removes the
	// override and returns the tenant to its plan.
	RatelimitDailyCap   *int `json:"ratelimit_daily_cap,omitempty"`
	RatelimitMonthlyCap *int `json:"ratelimit_monthly_cap,omitempty"`
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
	writeJSON(w, http.StatusOK, h.toTenantResponse(t))
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
	if req.BridgeOfflineTimeout != nil {
		v := *req.BridgeOfflineTimeout
		// 0 is "go back to the platform default" and is always allowed. Any
		// other value has to sit inside the platform's bounds: below a bridge's
		// own heartbeat the reaper flaps a healthy fleet offline, and far above
		// it a dead bridge reads as online for as long as somebody typed.
		if v != 0 && (v < h.bridgeTimeoutMin || v > h.bridgeTimeoutMax) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"bridge_offline_timeout must be 0 (platform default) or between %d and %d seconds",
				h.bridgeTimeoutMin, h.bridgeTimeoutMax))
			return
		}
		t.BridgeOfflineTimeout = v
	}
	if req.AuditRetentionDays != nil {
		v := *req.AuditRetentionDays
		// 0 is "the platform default" and always allowed. Anything else must
		// sit inside the bounds: the floor is what stops a tenant shortening
		// retention until the evidence of a security event in their own account
		// is gone before anyone looks at it.
		if v != 0 && (v < h.auditRetentionMin || v > h.auditRetentionMax) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"audit_retention_days must be 0 (platform default) or between %d and %d days",
				h.auditRetentionMin, h.auditRetentionMax))
			return
		}
		t.AuditRetentionDays = v
	}
	// Out-of-band command policy. 0 always means the platform default; anything
	// else sits inside the platform's bounds. Both ends matter: a ceiling of 1
	// makes a field kit unmanageable, and an unbounded one turns a stuck script
	// into a bill on the customer's own carrier account (MESHSAT-1121).
	for _, f := range []struct {
		name     string
		val      *int
		dst      *int
		min, max int
		unit     string
	}{
		{"oob_max_per_hour", req.OOBMaxPerHour, &t.OOBMaxPerHour, h.oobMaxPerHourMin, h.oobMaxPerHourMax, "frames per hour"},
		{"oob_sms_timeout_sec", req.OOBSMSTimeoutSec, &t.OOBSMSTimeoutSec, h.oobTimeoutMin, h.oobTimeoutMax, "seconds"},
		{"oob_sat_timeout_sec", req.OOBSatTimeoutSec, &t.OOBSatTimeoutSec, h.oobTimeoutMin, h.oobTimeoutMax, "seconds"},
	} {
		if f.val == nil {
			continue
		}
		v := *f.val
		if v != 0 && (v < f.min || v > f.max) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s must be 0 (platform default) or between %d and %d %s",
				f.name, f.min, f.max, f.unit))
			return
		}
		*f.dst = v
	}
	// The send budget. 0 is the platform default; anything from 1 up to the
	// ceiling is the owner's call, below the default included: they pay their
	// own carrier, so a low number is how they protect their own bill. The
	// ceiling exists to catch a typo, not to sell anything.
	capsChanged := false
	for _, f := range []struct {
		name       string
		val        *int
		dst        *int
		floor, max int
	}{
		{"ratelimit_daily_cap", req.RatelimitDailyCap, &t.RatelimitDailyCap, 1, h.sendCapDailyMax},
		{"ratelimit_monthly_cap", req.RatelimitMonthlyCap, &t.RatelimitMonthlyCap, 1, h.sendCapMonthlyMax},
	} {
		if f.val == nil {
			continue
		}
		v := *f.val
		if v != 0 && (v < f.floor || v < 1 || (f.max > 0 && v > f.max)) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s must be 0 (platform default) or between %d and %d messages per device",
				f.name, max(f.floor, 1), f.max))
			return
		}
		if *f.dst != v {
			capsChanged = true
		}
		*f.dst = v
	}
	if err := h.store.UpdateTenant(r.Context(), t); err != nil {
		slog.Error("tenant: update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	// The limiter caches a tenant's budget for 30 s on each replica; drop it
	// everywhere so the owner's new number is the one in force now.
	if capsChanged && h.forget != nil {
		h.forget(t.ID)
	}
	writeJSON(w, http.StatusOK, h.toTenantResponse(t))
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
	items, err := h.store.ListTenantSummaries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	includeDeleted := r.URL.Query().Get("include_deleted") == "1"
	out := make([]adminTenantResponse, 0, len(items))
	for i := range items {
		ts := &items[i]
		if ts.DeletedAt != nil && !includeDeleted {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(ts.ID+" "+ts.Slug+" "+ts.Name+" "+ts.OwnerEmail), q) {
			continue
		}
		out = append(out, h.toAdminTenantResponse(ts, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// adminTenantResponse is a tenant as the platform directory and the tenant
// detail page see it (MESHSAT-1366): the owner's own view plus the facts an
// operator needs and the customer never edits.
type adminTenantResponse struct {
	tenantResponse
	OwnerEmail string `json:"owner_email,omitempty"`
	Users      int    `json:"users"`
	Devices    int    `json:"devices"`
	Bridges    int    `json:"bridges"`
	// Used and Limit mirror internal/quota: devices plus bridges against the
	// plan's ceiling, -1 when the plan has none.
	Used                   int    `json:"used"`
	Limit                  int    `json:"limit"`
	OverLimit              bool   `json:"over_limit"`
	PlanExpiresAt          string `json:"plan_expires_at,omitempty"`
	BillingCountry         string `json:"billing_country,omitempty"`
	BillingCountryEvidence string `json:"billing_country_evidence,omitempty"`
	StripeCustomerID       string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID   string `json:"stripe_subscription_id,omitempty"`
	DeletedAt              string `json:"deleted_at,omitempty"`
	PurgeAt                string `json:"purge_at,omitempty"`
	LapseWarnedAt          string `json:"lapse_warned_at,omitempty"`
	// SupportAccess is the customer's consent for the platform to open this
	// workspace: present on the detail view, nil in the directory.
	SupportAccess *supportAccessResponse `json:"support_access,omitempty"`
}

func (h *TenantHandler) toAdminTenantResponse(ts *store.TenantSummary, grant *supportAccessResponse) adminTenantResponse {
	t := &ts.Tenant
	out := adminTenantResponse{
		tenantResponse: toTenantResponse(t),
		OwnerEmail:     ts.OwnerEmail, Users: ts.Users, Devices: ts.Devices, Bridges: ts.Bridges,
		Used:                   ts.Devices + ts.Bridges,
		Limit:                  plans.For(t.Plan).Devices,
		BillingCountry:         t.BillingCountry,
		BillingCountryEvidence: t.BillingCountryEvidence,
		StripeCustomerID:       t.StripeCustomerID,
		StripeSubscriptionID:   t.StripeSubscriptionID,
		SupportAccess:          grant,
	}
	out.OverLimit = out.Limit >= 0 && out.Used > out.Limit
	if t.PlanExpiresAt != nil {
		out.PlanExpiresAt = t.PlanExpiresAt.UTC().Format(time.RFC3339)
	}
	if t.DeletedAt != nil {
		out.DeletedAt = t.DeletedAt.UTC().Format(time.RFC3339)
		out.PurgeAt = t.DeletedAt.Add(store.PurgeGrace).UTC().Format(time.RFC3339)
	}
	if t.LapseWarnedAt != nil {
		out.LapseWarnedAt = t.LapseWarnedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// adminSummary finds one tenant's summary row; the grouped query is the one
// place the counts and the owner address are computed.
func (h *TenantHandler) adminSummary(r *http.Request, id string) (*store.TenantSummary, error) {
	items, err := h.store.ListTenantSummaries(r.Context())
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == id {
			return &items[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// AdminGet is one tenant with everything the detail page shows.
//
//	@Summary      Get a tenant (platform administrators only)
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      string  true  "Tenant ID"
//	@Success      200  {object}  adminTenantResponse
//	@Failure      404  {object}  map[string]string
//	@Router       /api/admin/tenants/{id} [get]
func (h *TenantHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	ts, err := h.adminSummary(r, chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	grant := supportAccessResponse{}
	if g, err := h.store.GetActiveSupportGrant(r.Context(), ts.ID); err == nil && g != nil {
		grant.Active = g.Active(time.Now())
		grant.ExpiresAt = g.ExpiresAt.UTC().Format(time.RFC3339)
		grant.CreatedAt = g.CreatedAt.UTC().Format(time.RFC3339)
		grant.CreatedByEmail = g.CreatedByEmail
		if g.UsedAt != nil {
			grant.UsedAt = g.UsedAt.UTC().Format(time.RFC3339)
			grant.UsedByEmail = g.UsedByEmail
		}
	}
	writeJSON(w, http.StatusOK, h.toAdminTenantResponse(ts, &grant))
}

// AdminListUsers lists a tenant's users for the platform (the tenant comes
// from the URL, never from X-Tenant-ID, so no support grant is needed to
// see WHO is on an account).
//
//	@Summary      List a tenant's users (platform administrators only)
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      string  true  "Tenant ID"
//	@Success      200  {array}   userResponse
//	@Router       /api/admin/tenants/{id}/users [get]
func (h *TenantHandler) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if t, err := h.store.GetTenant(r.Context(), id); err != nil || t == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	users, err := h.store.ListUsers(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	resp := make([]userResponse, len(users))
	for i := range users {
		resp[i] = toUserResponse(&users[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// AdminListAudit is the newest entries of a tenant's own audit chain.
//
//	@Summary      A tenant's recent audit entries (platform administrators only)
//	@Tags         admin
//	@Produce      json
//	@Param        id     path      string  true   "Tenant ID"
//	@Param        limit  query     int     false  "max entries (default 50)"
//	@Success      200    {array}   store.AuditEntry
//	@Router       /api/admin/tenants/{id}/audit [get]
func (h *TenantHandler) AdminListAudit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if t, err := h.store.GetTenant(r.Context(), id); err != nil || t == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	entries, err := h.store.ListAuditEntries(r.Context(), id, parseLimit(r, 50, maxListLimit))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list audit entries")
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
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
	before := *t
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
		// Setting a closed account back to active is how it is recovered
		// inside the grace period; the purge job keys on deleted_at, so the
		// stamp must go too or the account is destroyed on schedule anyway.
		if v == store.TenantActive {
			t.DeletedAt = nil
		}
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
	// The send budget, set by an operator for a customer. Refused if negative;
	// 0 clears. No upper bound here on purpose. The number is honoured as it is,
	// above or below the platform default: it is the tenant's own airtime.
	for _, o := range []struct {
		name string
		req  *int
		dst  *int
	}{
		{"ratelimit_daily_cap", req.RatelimitDailyCap, &t.RatelimitDailyCap},
		{"ratelimit_monthly_cap", req.RatelimitMonthlyCap, &t.RatelimitMonthlyCap},
	} {
		if o.req == nil {
			continue
		}
		if *o.req < 0 {
			writeError(w, http.StatusBadRequest, o.name+" must be 0 (use the plan) or positive")
			return
		}
		*o.dst = *o.req
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
	if changes := adminTenantChanges(&before, t); changes != "" {
		logPlatformAction(r, h.audit, t.ID, "tenant_admin_updated", changes)
	}
	writeJSON(w, http.StatusOK, toTenantResponse(t))
}

// adminTenantChanges names what an operator changed, old to new, for the
// audit row. Nothing changed is an empty string and no row.
func adminTenantChanges(before, after *store.Tenant) string {
	var parts []string
	add := func(name, from, to string) {
		if from != to {
			parts = append(parts, name+"="+from+">"+to)
		}
	}
	tstr := func(t *time.Time) string {
		if t == nil {
			return "none"
		}
		return t.UTC().Format(time.RFC3339)
	}
	add("name", before.Name, after.Name)
	add("plan", before.Plan, after.Plan)
	add("status", before.Status, after.Status)
	add("plan_expires_at", tstr(before.PlanExpiresAt), tstr(after.PlanExpiresAt))
	add("deleted_at", tstr(before.DeletedAt), tstr(after.DeletedAt))
	add("ratelimit_daily_cap", strconv.Itoa(before.RatelimitDailyCap), strconv.Itoa(after.RatelimitDailyCap))
	add("ratelimit_monthly_cap", strconv.Itoa(before.RatelimitMonthlyCap), strconv.Itoa(after.RatelimitMonthlyCap))
	add("oob_max_per_hour", strconv.Itoa(before.OOBMaxPerHour), strconv.Itoa(after.OOBMaxPerHour))
	add("oob_sms_timeout_sec", strconv.Itoa(before.OOBSMSTimeoutSec), strconv.Itoa(after.OOBSMSTimeoutSec))
	add("oob_sat_timeout_sec", strconv.Itoa(before.OOBSatTimeoutSec), strconv.Itoa(after.OOBSatTimeoutSec))
	return strings.Join(parts, " ")
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
