package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// OIDC login roles are derived from IdP group membership. The IdP is the
// identity and the approval gate; the Hub's users table is the RBAC source.
const (
	oidcStateCookie   = "meshsat_oidc"
	oidcStateTTL      = 10 * time.Minute
	oidcCallbackRoute = "/#/auth/callback"
	defaultTenantID   = "default"
)

// OIDCConfig configures the authorization-code login handler.
type OIDCConfig struct {
	// GroupsClaim is the ID-token claim carrying group names.
	GroupsClaim string
	// AdminGroup marks platform administrators (may act on every tenant).
	AdminGroup string
	// GroupRoles maps an IdP group name to a Hub role.
	GroupRoles map[string]string
	// BootstrapOwnerEmail attaches this (verified) email to the default tenant
	// as owner on first login: the migration path for the existing beta owner.
	BootstrapOwnerEmail string
	// StateKey signs the state cookie (the session signing key is fine).
	StateKey []byte
	// TenantEnforce mirrors HUB_TENANT_ENFORCE; when false, users with no
	// tenant land in the default tenant instead of a fresh one.
	TenantEnforce bool
	// SignupURL and CommunityURL are public links shown by the SPA (login
	// page "Request beta access", pending-approval state). Both optional.
	SignupURL    string
	CommunityURL string
	RecoveryURL  string
	// PasswordChangeURL and MFASetupURL are the identity provider's
	// self-service flows for a signed-in customer.
	PasswordChangeURL string
	MFASetupURL       string
}

// OIDCHandler implements GET /api/auth/oidc/login, GET /api/auth/oidc/callback
// and GET /api/auth/config, and resolves provider subjects for the middleware.
type OIDCHandler struct {
	store  store.Store
	login  *LoginHandler
	client *hubauth.OIDCClient
	cfg    OIDCConfig
	modes  []string
}

// NewOIDCHandler wires the OIDC code flow onto an existing login handler.
func NewOIDCHandler(s store.Store, login *LoginHandler, client *hubauth.OIDCClient, cfg OIDCConfig, modes []string) *OIDCHandler {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.GroupRoles == nil {
		cfg.GroupRoles = map[string]string{
			"meshsat-owner":    hubauth.RoleOwner,
			"meshsat-operator": hubauth.RoleOperator,
			"meshsat-viewer":   hubauth.RoleViewer,
		}
	}
	return &OIDCHandler{store: s, login: login, client: client, cfg: cfg, modes: modes}
}

// authConfigResponse is returned by GET /api/auth/config.
type authConfigResponse struct {
	Modes        []string `json:"modes"`
	OIDCLoginURL string   `json:"oidc_login_url,omitempty"`
	SignupURL    string   `json:"signup_url,omitempty"`
	CommunityURL string   `json:"community_url,omitempty"`
	// RecoveryURL is the identity provider's password reset flow. It has
	// existed and been bound to the brand since MESHSAT-978, but nothing in the
	// Hub linked to it, so the only way to reach it was to already be on
	// authentik's own sign-in page and notice the link there.
	RecoveryURL string `json:"recovery_url,omitempty"`
	// PasswordChangeURL and MFASetupURL are the self-service flows a signed-in
	// customer can reach. authentik's settings page is closed to `external`
	// users, so without these there is no route to either from anywhere.
	PasswordChangeURL string `json:"password_change_url,omitempty"`
	MFASetupURL       string `json:"mfa_setup_url,omitempty"`
}

// Config tells the SPA which login methods exist.
// @Summary      Login methods available on this Hub
// @Tags         auth
// @Produce      json
// @Success      200  {object}  authConfigResponse
// @Router       /api/auth/config [get]
func (h *OIDCHandler) Config(w http.ResponseWriter, r *http.Request) {
	resp := authConfigResponse{Modes: h.modes, CommunityURL: h.cfg.CommunityURL}
	if h.client != nil {
		resp.OIDCLoginURL = "/api/auth/oidc/login"
		resp.SignupURL = h.cfg.SignupURL
		resp.RecoveryURL = h.cfg.RecoveryURL
		resp.PasswordChangeURL = h.cfg.PasswordChangeURL
		resp.MFASetupURL = h.cfg.MFASetupURL
	}
	writeJSON(w, http.StatusOK, resp)
}

// AuthConfigHandler serves GET /api/auth/config for Hubs without OIDC.
func AuthConfigHandler(modes []string, communityURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, authConfigResponse{Modes: modes, CommunityURL: communityURL})
	}
}

// safeNext accepts only same-origin, absolute-path targets.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return ""
	}
	if strings.ContainsAny(next, "\r\n") {
		return ""
	}
	return next
}

// Login starts the authorization-code flow.
// @Summary      Redirect to the identity provider
// @Tags         auth
// @Param        next  query  string  false  "Same-origin path to return to after login"
// @Success      302
// @Failure      503  {object}  map[string]string
// @Router       /api/auth/oidc/login [get]
func (h *OIDCHandler) Login(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "oidc login not configured"})
		return
	}
	st, err := hubauth.NewOIDCState(safeNext(r.URL.Query().Get("next")), oidcStateTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	authURL, err := h.client.AuthURL(r.Context(), st)
	if err != nil {
		slog.Error("oidc: build authorization url", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "identity provider unavailable"})
		return
	}
	encoded, err := hubauth.EncodeState(st, h.cfg.StateKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    encoded,
		Path:     "/api/auth/oidc",
		MaxAge:   int(oidcStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (h *OIDCHandler) clearStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/api/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
}

// failRedirect sends the browser to the SPA callback with an error code. No
// detail leaves the server; the code is one of a small fixed vocabulary.
func (h *OIDCHandler) failRedirect(w http.ResponseWriter, r *http.Request, code string) {
	h.clearStateCookie(w, r)
	http.Redirect(w, r, oidcCallbackRoute+"?error="+url.QueryEscape(code), http.StatusFound)
}

// Callback completes the authorization-code flow and issues a Hub session.
// @Summary      OIDC callback
// @Tags         auth
// @Param        code   query  string  true   "Authorization code"
// @Param        state  query  string  true   "State"
// @Success      302
// @Router       /api/auth/oidc/callback [get]
func (h *OIDCHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "oidc login not configured"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil {
		h.failRedirect(w, r, "state")
		return
	}
	st, err := hubauth.DecodeState(cookie.Value, h.cfg.StateKey)
	if err != nil || q.Get("state") == "" || q.Get("state") != st.State {
		h.failRedirect(w, r, "state")
		return
	}
	if e := q.Get("error"); e != "" {
		slog.Warn("oidc: provider returned error", "error", e)
		if e == "access_denied" {
			h.failRedirect(w, r, "denied")
			return
		}
		h.failRedirect(w, r, "provider")
		return
	}
	code := q.Get("code")
	if code == "" {
		h.failRedirect(w, r, "state")
		return
	}
	rawID, err := h.client.Exchange(r.Context(), code, st.Verifier)
	if err != nil {
		slog.Warn("oidc: code exchange failed", "error", err)
		h.failRedirect(w, r, "exchange")
		return
	}
	claims, err := h.client.VerifyIDToken(rawID, st.Nonce)
	if err != nil {
		slog.Warn("oidc: id token rejected", "error", err)
		h.failRedirect(w, r, "token")
		return
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	emailVerified, _ := claims["email_verified"].(bool)
	name, _ := claims["name"].(string)
	if name == "" {
		name, _ = claims["preferred_username"].(string)
	}
	if sub == "" || email == "" {
		h.failRedirect(w, r, "token")
		return
	}
	// Where the buyer is, for VAT. Declared on the enrollment form, observed at
	// signup, and carried here because the tenant is created from these claims
	// and nothing else (MESHSAT-1016).
	country, _ := claims["country"].(string)
	signupIP, _ := claims["signup_ip"].(string)
	groups := hubauth.StringSliceClaim(claims, h.cfg.GroupsClaim)
	role, platformAdmin := h.roleFromGroups(groups)
	if role == "" {
		// Registered but not approved: nothing is created.
		slog.Info("oidc: login without a mapped group", "email", email)
		h.failRedirect(w, r, "pending_approval")
		return
	}
	issuer := h.client.Provider.ExpectedIssuer()

	user, tenantID, err := h.resolveUser(r.Context(), issuer, sub, email, emailVerified, name, role,
		buyerLocation{Country: country, SignupIP: signupIP})
	if err != nil {
		slog.Error("oidc: resolve user", "error", err, "email", email)
		h.failRedirect(w, r, "provision")
		return
	}
	if !user.Enabled {
		h.failRedirect(w, r, "disabled")
		return
	}
	if err := h.store.LinkOIDCIdentity(r.Context(), &store.OIDCIdentity{
		Issuer: issuer, Subject: sub, UserID: user.ID, TenantID: tenantID, Email: email, PlatformAdmin: platformAdmin,
	}); err != nil {
		slog.Error("oidc: link identity", "error", err)
		h.failRedirect(w, r, "provision")
		return
	}
	if _, err := h.login.issueSession(w, r, user, tenantID, platformAdmin); err != nil {
		slog.Error("oidc: issue session", "error", err)
		h.failRedirect(w, r, "session")
		return
	}
	h.clearStateCookie(w, r)
	if h.login.audit != nil {
		h.login.auditLog(r, "oidc_login_success", email, clientIPFromRequest(r))
	}
	slog.Info("oidc: login success", "email", email, "tenant", tenantID, "role", user.Role, "platform_admin", platformAdmin)
	target := oidcCallbackRoute
	if n := safeNext(st.Next); n != "" {
		target += "?next=" + url.QueryEscape(n)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// ownsTenant reports whether this user is the tenant's owner of record.
//
// It fails OPEN -- an unreadable tenant is treated as owned, so the role is
// left alone. The asymmetry is deliberate: wrongly demoting locks a person out
// of the tenant they own and needs an operator to undo, while wrongly skipping
// a demotion leaves a stale role until the next sign-in.
func (h *OIDCHandler) ownsTenant(ctx context.Context, tenantID, userID string) bool {
	t, err := h.store.GetTenant(ctx, tenantID)
	if err != nil {
		slog.Warn("oidc: could not read the tenant to check ownership; leaving the role alone",
			"tenant", tenantID, "error", err)
		return true
	}
	if t == nil {
		return true
	}
	return t.OwnerUserID != "" && t.OwnerUserID == userID
}

// roleFromGroups picks the highest mapped role; the admin group implies owner.
func (h *OIDCHandler) roleFromGroups(groups []string) (role string, admin bool) {
	rank := map[string]int{hubauth.RoleViewer: 1, hubauth.RoleOperator: 2, hubauth.RoleOwner: 3}
	best := 0
	for _, g := range groups {
		if h.cfg.AdminGroup != "" && g == h.cfg.AdminGroup {
			admin = true
			if best < rank[hubauth.RoleOwner] {
				best, role = rank[hubauth.RoleOwner], hubauth.RoleOwner
			}
			continue
		}
		if mapped, ok := h.cfg.GroupRoles[g]; ok && rank[mapped] > best {
			best, role = rank[mapped], mapped
		}
	}
	return role, admin
}

// resolveUser finds or creates the local user for a verified login, in order:
// linked identity, bootstrap owner (default tenant), pending invite (joins the
// inviting tenant), else a new tenant owned by the user.
// buyerLocation is where a new account says it is and where it was seen from.
// Two independent pieces, which is what the VAT rules ask for on a consumer
// supply: one declared, one observed.
type buyerLocation struct {
	Country  string
	SignupIP string
}

// evidence renders the pair for the record, so the reasoning behind a country
// survives in a form a person can read a year later.
func (b buyerLocation) evidence() string {
	if b.Country == "" && b.SignupIP == "" {
		return ""
	}
	return "declared " + b.Country + ", seen from " + b.SignupIP + " at sign-up"
}

func (h *OIDCHandler) resolveUser(ctx context.Context, issuer, sub, email string, emailVerified bool, name, role string, loc buyerLocation) (*store.LocalUser, string, error) {
	if ident, err := h.store.GetOIDCIdentity(ctx, issuer, sub); err == nil && ident != nil {
		u, err := h.store.GetUserByID(ctx, ident.TenantID, ident.UserID)
		if err != nil {
			return nil, "", fmt.Errorf("linked user %s missing: %w", ident.UserID, err)
		}
		changed := false
		if name != "" && u.Name != name {
			u.Name = name
			changed = true
		}
		// A tenant's own owner is never demoted by an identity-provider group.
		// Approving somebody as viewer or operator still gives them their own
		// tenant with themselves as owner, and this line used to take it back
		// on their SECOND sign-in: they owned a tenant they could not
		// administer -- no users, no API keys, no plan -- and nothing in the UI
		// said why. Everyone else's role stays the provider's to decide (owner
		// ruling, 2026-09-10).
		if u.Role != role && !h.ownsTenant(ctx, ident.TenantID, u.ID) {
			u.Role = role
			changed = true
		}
		if changed {
			if err := h.store.UpdateUser(ctx, ident.TenantID, u); err != nil {
				return nil, "", err
			}
		}
		return u, ident.TenantID, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}

	// Email-keyed paths require a verified address: the IdP asserts it.
	if !emailVerified {
		return nil, "", errors.New("email not verified by the identity provider")
	}

	if h.cfg.BootstrapOwnerEmail != "" && strings.EqualFold(h.cfg.BootstrapOwnerEmail, email) {
		return h.findOrCreateUser(ctx, defaultTenantID, email, name, hubauth.RoleOwner)
	}

	if inv, err := h.store.GetPendingInviteByEmail(ctx, email); err == nil && inv != nil {
		if inv.ExpiresAt.After(time.Now()) {
			u, tid, err := h.findOrCreateUser(ctx, inv.TenantID, email, name, inv.Role)
			if err != nil {
				return nil, "", err
			}
			if err := h.store.AcceptInvite(ctx, inv.ID); err != nil {
				return nil, "", err
			}
			return u, tid, nil
		}
	}

	if !h.cfg.TenantEnforce {
		// Single-tenant deployments keep everyone in the default tenant.
		return h.findOrCreateUser(ctx, defaultTenantID, email, name, role)
	}

	// New account: a tenant of its own, the user is its owner.
	tenant, err := h.createTenantFor(ctx, email, name, loc)
	if err != nil {
		return nil, "", err
	}
	u, tid, err := h.findOrCreateUser(ctx, tenant.ID, email, name, hubauth.RoleOwner)
	if err != nil {
		return nil, "", err
	}
	// Claim the subject before this tenant is anybody's. Two callbacks for one
	// new subject arrive together often enough -- a double submit, a browser
	// retrying -- and both get this far: the slug loop hands the second one
	// "alice-2" rather than colliding, and the upsert at the end of Callback
	// would then repoint the subject at it, leaving the first tenant fully
	// populated, owned, counted by billing and reachable by nobody. The loser
	// discards what it just built and adopts the winner (MESHSAT-1006).
	winner, claimed, err := h.store.ClaimOIDCIdentity(ctx, &store.OIDCIdentity{
		Issuer: issuer, Subject: sub, UserID: u.ID, TenantID: tenant.ID, Email: email,
	})
	if err != nil {
		return nil, "", err
	}
	if winner != nil && winner.TenantID == tenant.ID {
		// The subject already points at the tenant this request just built, so
		// this request is the winner however the claim was reported. Without
		// this, a driver that cannot report rows affected would have us
		// discard our own tenant and then adopt an identity pointing straight
		// at it -- a signed-in user inside a deleted tenant, refused
		// everything by the tenant middleware.
		claimed = true
	}
	if !claimed {
		// Nothing of value is lost: this tenant is seconds old, holds one user
		// -- the same person -- and no devices. Soft delete keeps it visible to
		// an operator for the grace period rather than vanishing silently.
		if err := h.store.SoftDeleteTenant(ctx, tenant.ID, time.Now().UTC()); err != nil {
			slog.Warn("oidc: could not discard the tenant that lost a provisioning race",
				"tenant", tenant.ID, "kept", winner.TenantID, "error", err)
		}
		slog.Info("oidc: two callbacks provisioned the same new account; kept the first",
			"kept", winner.TenantID, "discarded", tenant.ID, "email", email)
		wu, err := h.store.GetUserByID(ctx, winner.TenantID, winner.UserID)
		if err != nil {
			return nil, "", err
		}
		return wu, winner.TenantID, nil
	}
	tenant.OwnerUserID = u.ID
	if err := h.store.UpdateTenant(ctx, tenant); err != nil {
		return nil, "", err
	}
	return u, tid, nil
}

func (h *OIDCHandler) findOrCreateUser(ctx context.Context, tenantID, email, name, role string) (*store.LocalUser, string, error) {
	if u, err := h.store.GetUserByEmail(ctx, tenantID, email); err == nil && u != nil {
		// A pre-existing row (migrated local account) takes the IdP's display name.
		if name != "" && u.Name != name {
			u.Name = name
			if err := h.store.UpdateUser(ctx, tenantID, u); err != nil {
				return nil, "", err
			}
		}
		return u, tenantID, nil
	}
	id, err := generateUserID()
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		name = email
	}
	now := time.Now().UTC()
	u := &store.LocalUser{ID: id, Email: email, Name: name, Role: role, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := h.store.CreateUser(ctx, tenantID, u); err != nil {
		return nil, "", err
	}
	return u, tenantID, nil
}

var slugClean = regexp.MustCompile(`[^a-z0-9]+`)

// tenantSlug derives a URL-safe slug from the email local part.
func tenantSlug(email string) string {
	local := email
	if i := strings.Index(email, "@"); i > 0 {
		local = email[:i]
	}
	s := strings.Trim(slugClean.ReplaceAllString(strings.ToLower(local), "-"), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" || store.ReservedTenantIDs[s] {
		s = "tenant-" + s
	}
	return s
}

func (h *OIDCHandler) createTenantFor(ctx context.Context, email, name string, loc buyerLocation) (*store.Tenant, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	base := tenantSlug(email)
	slug := base
	for i := 2; i < 50; i++ {
		if existing, err := h.store.GetTenantBySlug(ctx, slug); err != nil || existing == nil {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	displayName := name
	if displayName == "" {
		displayName = email
	}
	now := time.Now().UTC()
	t := &store.Tenant{ID: "t_" + id[:16], Slug: slug, Name: displayName, Plan: plans.Free, Status: "active", CreatedAt: now, UpdatedAt: now,
		BillingCountry:         strings.ToUpper(strings.TrimSpace(loc.Country)),
		BillingCountryEvidence: loc.evidence()}
	if err := h.store.CreateTenant(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// ResolveSubject implements hubauth.SubjectResolver for provider-issued bearer
// tokens: only subjects that completed a browser login are accepted, and their
// role/tenant come from the users table.
func (h *OIDCHandler) ResolveSubject(ctx context.Context, issuer, subject string) (*hubauth.User, error) {
	ident, err := h.store.GetOIDCIdentity(ctx, issuer, subject)
	if err != nil {
		return nil, err
	}
	u, err := h.store.GetUserByID(ctx, ident.TenantID, ident.UserID)
	if err != nil {
		return nil, err
	}
	if !u.Enabled {
		return nil, errors.New("user disabled")
	}
	return &hubauth.User{ID: u.ID, Email: u.Email, Name: u.Name, Roles: []string{u.Role}, TenantID: ident.TenantID, PlatformAdmin: ident.PlatformAdmin}, nil
}

func clientIPFromRequest(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
