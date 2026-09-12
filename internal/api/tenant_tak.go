package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Hosted TAK, from the customer's side (MESHSAT-1037): turn it on, add the people
// who should see the map, take one off again.
//
// # Why internal/takhosted is behind interfaces here
//
// internal/routing imports this package, and routing is on an ingest path. An
// import of internal/takhosted would therefore put the Kubernetes custom-resource
// client and its in-cluster REST config behind every satellite message the Hub
// receives. That is the coupling that already forces internal/quota's invariant
// test to scan source text rather than the dependency graph (MESHSAT-992), and
// one instance of it is enough. So this package names the two things it needs and
// cmd/meshsat-hub supplies them.
//
// *quota.Checker, by contrast, is imported directly: devices.go, bridges.go and
// tenant_usage.go already do, so it adds no reach that is not there.

// TAKProvisioner creates the tenant's hosted OpenTAKServer. Implemented by
// takhosted.Provisioner.
type TAKProvisioner interface {
	// EnableTAK is idempotent and returns the instance label.
	EnableTAK(ctx context.Context, tenantID string) (string, error)
}

// TAKAccountOutcome is what the operator made of an account request.
type TAKAccountOutcome int

const (
	// TAKAccountApplied means the instance now matches the request.
	TAKAccountApplied TAKAccountOutcome = iota
	// TAKAccountPending means the operator has not acted yet. Normal: it
	// reconciles on a ticker, so a fresh request waits a few seconds.
	TAKAccountPending
	// TAKAccountRefused is terminal and carries a reason the customer can act on.
	TAKAccountRefused
)

// TAKAccounts asks the operator for OpenTAKServer accounts. Implemented by an
// adapter over takhosted.AccountKeeper, which translates its sentinel errors into
// the outcomes above.
//
// The Hub cannot create these accounts itself: every /api/user/ route on an
// instance needs an administrator session, and that password lives in a Secret
// the Hub's Role deliberately cannot read.
type TAKAccounts interface {
	Ensure(ctx context.Context, label, username string) (TAKAccountOutcome, string, error)
	Deactivate(ctx context.Context, label, username string) (TAKAccountOutcome, string, error)
}

// takUsernamePattern is what OpenTAKServer accepts, duplicated here so the
// refusal names the rule instead of arriving as a 400 from the instance or an
// admission failure from the API server. No hyphens: upstream's own validator
// rejects them.
var takUsernamePattern = regexp.MustCompile(`^[a-z0-9]{3,32}$`)

// TenantTAKHandler serves /api/tenant/tak.
type TenantTAKHandler struct {
	store store.Store
	audit *audit.Service
	quota *quota.Checker
	prov  TAKProvisioner
	accts TAKAccounts
	certs TAKCerts
	// port is the public TAK SSL port a phone connects to.
	port int
	// publicHost is the name a phone dials. It goes into the enrolment package's
	// connectString, which is the one value that decides whether a phone reaches
	// us at all -- the instance's own Host is an in-cluster service name no phone
	// can resolve.
	publicHost string
	log        *slog.Logger
}

// NewTenantTAKHandler creates the handler. It is inert until SetTAK supplies the
// provisioner and the account keeper, which is what a Hub running without hosted
// TAK wants.
func NewTenantTAKHandler(s store.Store, a *audit.Service, q *quota.Checker, port int, publicHost string) *TenantTAKHandler {
	return &TenantTAKHandler{
		store: s, audit: a, quota: q,
		port: port, publicHost: publicHost,
		log: slog.Default(),
	}
}

// SetTAK attaches the hosted-TAK machinery, in the shape of DeviceHandler.SetQuota.
func (h *TenantTAKHandler) SetTAK(p TAKProvisioner, a TAKAccounts) {
	h.prov, h.accts = p, a
}

type takStatusResponse struct {
	// Enabled is whether this tenant has a server at all.
	Enabled bool `json:"enabled"`
	// Phase is the operator's own word for where it has got to: Provisioning,
	// Ready, Failed and so on.
	Phase string `json:"phase,omitempty"`
	// Port is the public TAK SSL port. There is deliberately no host here: the
	// instance's address is an in-cluster service name that no phone can reach,
	// and publishing it would tell a customer something both useless and internal.
	// A phone is configured from its enrollment package, not from this.
	Port int `json:"port,omitempty"`
	// Users is the account meter.
	Users     int    `json:"users"`
	Limit     int    `json:"limit"`     // -1 when the plan has no ceiling
	Remaining int    `json:"remaining"` // -1 when unlimited
	Plan      string `json:"plan"`
}

type takUserResponse struct {
	Username string `json:"username"`
	Callsign string `json:"callsign,omitempty"`
	Active   bool   `json:"active"`
	// CertSerial is present once a phone has enrolled. The enrollment token hash
	// is never here: store.TAKUser marks it json:"-" and it is in
	// store.RedactedInExport for the same reason.
	CertSerial string `json:"cert_serial,omitempty"`
}

type addTAKUserRequest struct {
	Username string `json:"username"`
	Callsign string `json:"callsign"`
}

// Status reports whether the tenant has a TAK server and how many accounts it may
// have.
//
// @Summary      Hosted TAK status
// @Tags         tenant
// @Produce      json
// @Success      200  {object}  takStatusResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/tenant/tak [get]
func (h *TenantTAKHandler) Status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	out := takStatusResponse{Port: h.port}
	if u, err := h.quota.TAKUsage(ctx, tenantID); err == nil {
		out.Users, out.Limit, out.Remaining, out.Plan = u.Users, u.Limit, u.Remaining, u.Plan
	}

	inst, err := h.store.GetTAKInstance(ctx, tenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Not an error. Most tenants will never turn TAK on.
		out.Enabled = false
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read the TAK status")
		return
	default:
		out.Enabled = true
		out.Phase = inst.Phase
	}
	writeJSON(w, http.StatusOK, out)
}

// Enable creates the tenant's own OpenTAKServer (owner-only, idempotent).
//
// @Summary      Turn hosted TAK on
// @Tags         tenant
// @Produce      json
// @Success      202  {object}  takStatusResponse  "being built"
// @Success      200  {object}  takStatusResponse  "already on"
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/tak [post]
func (h *TenantTAKHandler) Enable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	if h.prov == nil {
		writeError(w, http.StatusServiceUnavailable, "hosted TAK is not available on this Hub")
		return
	}

	existing, err := h.store.GetTAKInstance(ctx, tenantID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "could not read the TAK status")
		return
	}
	already := existing != nil && err == nil

	if _, err := h.prov.EnableTAK(ctx, tenantID); err != nil {
		h.log.Error("tak: enabling hosted TAK failed", "tenant", tenantID, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"could not start building your TAK server; please try again shortly")
		return
	}

	if !already {
		h.logAudit(r, tenantID, "tak_enabled", "hosted TAK requested")
	}

	out := takStatusResponse{Enabled: true, Phase: "Provisioning", Port: h.port}
	if existing != nil {
		out.Phase = existing.Phase
	}
	if u, err := h.quota.TAKUsage(ctx, tenantID); err == nil {
		out.Users, out.Limit, out.Remaining, out.Plan = u.Users, u.Limit, u.Remaining, u.Plan
	}
	if already {
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// ListUsers returns the tenant's TAK accounts.
//
// @Summary      List TAK accounts
// @Tags         tenant
// @Produce      json
// @Success      200  {array}   takUserResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/tenant/tak/users [get]
func (h *TenantTAKHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := h.store.ListTAKUsers(ctx, hubauth.TenantIDFromContext(ctx))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list TAK accounts")
		return
	}
	out := make([]takUserResponse, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		out = append(out, takUserResponse{
			Username: u.Username, Callsign: u.Callsign,
			Active: u.Active, CertSerial: u.CertSerial,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// AddUser creates a TAK account (owner-only).
//
// @Summary      Add a TAK account
// @Tags         tenant
// @Accept       json
// @Produce      json
// @Param        body  body      addTAKUserRequest  true  "Account"
// @Success      202   {object}  takUserResponse  "being created"
// @Success      200   {object}  takUserResponse  "already there"
// @Failure      400   {object}  map[string]string
// @Failure      402   {object}  map[string]string  "plan ceiling reached"
// @Failure      409   {object}  map[string]string
// @Failure      422   {object}  map[string]string  "refused by the instance"
// @Router       /api/tenant/tak/users [post]
func (h *TenantTAKHandler) AddUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	if h.accts == nil {
		writeError(w, http.StatusServiceUnavailable, "hosted TAK is not available on this Hub")
		return
	}

	var req addTAKUserRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Trimmed but NOT lowercased, deliberately. This name becomes the common name
	// on a phone's certificate and the account OpenTAKServer matches it against, so
	// silently turning FieldTeam1 into fieldteam1 would create an identity the
	// customer did not ask for and believes is spelled differently -- and two
	// spellings would collide onto one account, so a later attempt would answer
	// "already exists" for a name nobody created. The refusal below names the rule;
	// accepting and rewriting would make that message a lie.
	//
	// It also keeps this in step with the operator, whose own validation reads the
	// username exactly as given.
	username := strings.TrimSpace(req.Username)
	if !takUsernamePattern.MatchString(username) {
		writeError(w, http.StatusBadRequest,
			"a TAK username must be 3 to 32 lowercase letters or digits, with no hyphens, "+
				"because OpenTAKServer refuses anything else")
		return
	}
	callsign := strings.TrimSpace(req.Callsign)
	if len(callsign) > 64 {
		writeError(w, http.StatusBadRequest, "callsign must be 64 characters or fewer")
		return
	}

	inst, err := h.store.GetTAKInstance(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "turn TAK on first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the TAK status")
		return
	}

	// An account that is already there and active is success, so a double-click
	// does not read as an error. One that was DEACTIVATED is refused with the
	// reason: re-enabling needs an OpenTAKServer route that has not been verified,
	// and pretending otherwise would leave somebody unable to connect while the
	// Hub said they could.
	if existing, err := h.store.GetTAKUser(ctx, tenantID, username); err == nil && existing != nil {
		if existing.Active {
			writeJSON(w, http.StatusOK, takUserResponse{
				Username: existing.Username, Callsign: existing.Callsign,
				Active: true, CertSerial: existing.CertSerial,
			})
			return
		}
		writeError(w, http.StatusConflict,
			"that account was removed and cannot be re-enabled yet; add a new one under a "+
				"different username")
		return
	}

	// The ceiling gates ADDING an account and nothing else. It never touches a
	// phone already connected, its positions, or anything on the SOS path.
	if ok, why := h.quota.AllowsTAKUser(ctx, tenantID); !ok {
		writeError(w, http.StatusPaymentRequired, why)
		return
	}

	outcome, message, err := h.accts.Ensure(ctx, inst.Label, username)
	if err != nil {
		h.log.Error("tak: asking for a TAK account failed",
			"tenant", tenantID, "username", username, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"could not create the account just now; please try again shortly")
		return
	}
	if outcome == TAKAccountRefused {
		writeError(w, http.StatusUnprocessableEntity, message)
		return
	}

	if err := h.store.CreateTAKUser(ctx, tenantID, &store.TAKUser{
		TenantID: tenantID, Username: username, Callsign: callsign, Active: true,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the TAK account")
		return
	}
	h.logAudit(r, tenantID, "tak_user_added", "username="+username)

	body := takUserResponse{Username: username, Callsign: callsign, Active: true}
	if outcome == TAKAccountApplied {
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusAccepted, body)
}

// RemoveUser deactivates a TAK account (owner-only).
//
// Deactivation, not deletion: the EUD rows, the position history and the audit
// trail all reference the user, and removing a teammate is not a request to
// rewrite the map's history.
//
// @Summary      Remove a TAK account
// @Tags         tenant
// @Produce      json
// @Param        username  path      string  true  "Account"
// @Success      202       {object}  map[string]string  "being removed"
// @Success      200       {object}  map[string]string  "removed"
// @Failure      404       {object}  map[string]string
// @Failure      422       {object}  map[string]string
// @Router       /api/tenant/tak/users/{username} [delete]
func (h *TenantTAKHandler) RemoveUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	if h.accts == nil {
		writeError(w, http.StatusServiceUnavailable, "hosted TAK is not available on this Hub")
		return
	}
	// As given, for the same reason AddUser does not lowercase: the URL must name
	// the account that exists, not a case-folded guess at it.
	username := chi.URLParam(r, "username")
	if !takUsernamePattern.MatchString(username) {
		writeError(w, http.StatusBadRequest, "not a TAK username")
		return
	}

	existing, err := h.store.GetTAKUser(ctx, tenantID, username)
	if err != nil || existing == nil {
		writeError(w, http.StatusNotFound, "no such TAK account")
		return
	}
	inst, err := h.store.GetTAKInstance(ctx, tenantID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no TAK server for this tenant")
		return
	}

	outcome, message, err := h.accts.Deactivate(ctx, inst.Label, username)
	if err != nil {
		h.log.Error("tak: deactivating a TAK account failed",
			"tenant", tenantID, "username", username, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"could not remove the account just now; please try again shortly")
		return
	}
	if outcome == TAKAccountRefused {
		writeError(w, http.StatusUnprocessableEntity, message)
		return
	}

	// The row is marked inactive whatever the operator has got to, because the
	// Hub's own authorizer reads it and refuses the connection BEFORE the instance
	// is reached. So the phone stops being admitted immediately, and the instance
	// catches up.
	existing.Active = false
	if err := h.store.UpdateTAKUser(ctx, tenantID, existing); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the removal")
		return
	}
	h.logAudit(r, tenantID, "tak_user_removed", "username="+username)

	status := http.StatusAccepted
	if outcome == TAKAccountApplied {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]string{"status": "removed", "username": username})
}

// logAudit records a TAK action beside takfront's own tak_stream_* entries. A
// failure to write one never fails the customer's request.
func (h *TenantTAKHandler) logAudit(r *http.Request, tenantID, action, detail string) {
	if h.audit == nil {
		return
	}
	actor := operatorEmail(r)
	if err := h.audit.Log(r.Context(), tenantID, action, actor, detail, clientIPFromRequest(r)); err != nil {
		h.log.Warn("tak: audit write failed", "action", action, "tenant", tenantID, "error", err)
	}
}
