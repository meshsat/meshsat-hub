package api

import (
	"context"
	"log/slog"
	"net/http"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/integrations"
)

// Capabilities answers one question the SPA had no way to ask: for THIS tenant,
// is this feature actually going to do anything?
//
// It exists because of the defect MESHSAT-1121 started from. Six features were
// switched off platform-wide by an environment variable and not one of them told
// the customer. The worst was Notifications: the endpoints were registered
// unconditionally, saving a target answered HTTP 200, and `escalation.New`
// silently substituted a LogNotifier when no backend was configured -- so an
// alert became a log line on a server the customer cannot read. There was no
// 404 and no 503 anywhere in the path, which is precisely why nothing detected
// it.
//
// Now that every one of those features is a per-tenant provider account, the
// question has a second half: a backend can exist and this tenant still have no
// account on it, which is the ordinary state of a new customer. Both halves are
// the same answer to the SPA -- "configured, or not, and here is where to go".
//
// # Why this replaces regexing error strings
//
// `OtaView.vue` and `EmailView.vue` each decided availability with
// `/not found|404/i` against an error message. That was fragile in the way that
// matters: a caller without the platform-admin role gets 403, the regex misses
// it, `unavailable` stays false, and the page renders an empty table. The
// customer is told there is no data when the truth is they are not allowed to
// look. A string match on an error message is not a capability check.
//
// # Why it is authenticated, unlike /api/auth/config
//
// The plan for this called for an auth-exempt endpoint in the shape of
// `/api/auth/config`. It is authenticated instead, deliberately: the answer is
// per tenant, so it needs a tenant, and the nav that consumes it is only ever
// rendered after sign-in. An auth-exempt version would also publish the
// operator's backend inventory to anyone who asked, which buys nothing.
type Capability struct {
	Feature string `json:"feature"`
	Label   string `json:"label"`
	// Providers are the integrations providers that can satisfy this feature.
	// Any ONE of them is enough -- notifications are delivered by Apprise or by
	// ntfy, and a tenant with either is configured.
	Providers []string `json:"providers"`
	// Configured is the whole point: does an account this tenant can use exist.
	Configured bool `json:"configured"`
	// Platform is true when what satisfies it is the operator's own account
	// rather than one this tenant entered. Only ever true for the default
	// tenant -- integrations.ForTenant returns nil for everybody else, which is
	// the guarantee this field depends on.
	Platform bool `json:"platform"`
	// Reason is shown to the customer when Configured is false. It says what
	// will silently not happen, because "unavailable" on its own reads as a
	// fault in the Hub rather than as something they can fix.
	Reason string `json:"reason,omitempty"`
}

// featureCapabilities maps a feature the UI has a page for onto the provider
// accounts that make it real.
//
// Adding a feature page without adding a row here is caught by
// TestEveryGatedFeatureHasACapability, which reads the Vue sources. That test is
// the reason this table cannot quietly fall behind the UI the way the settings
// audit did.
var featureCapabilities = []Capability{
	{
		Feature:   "notifications",
		Label:     "Notifications",
		Providers: []string{integrations.ProviderApprise, integrations.ProviderNtfy},
		Reason:    "Notification targets are saved, but nothing delivers them until you add an Apprise or ntfy server. Add one under Settings -> Integrations.",
	},
	{
		Feature:   "email",
		Label:     "Email gateway",
		Providers: []string{integrations.ProviderEmail},
		Reason:    "PGP email is not configured for this account. Add your SMTP details under Settings -> Integrations to send, or leave them empty to only receive.",
	},
	{
		Feature:   "ota",
		Label:     "OTA firmware",
		Providers: []string{integrations.ProviderHawkbit},
		Reason:    "Firmware rollouts need a hawkBit server. Add one under Settings -> Integrations.",
	},
	{
		Feature:   "wireguard",
		Label:     "WireGuard",
		Providers: []string{integrations.ProviderWireGuard},
		Reason:    "Device tunnels need a wg-easy server. Add one under Settings -> Integrations and the Hub will create a peer for each device you register.",
	},
	{
		Feature:   "aprs",
		Label:     "APRS-IS",
		Providers: []string{integrations.ProviderAPRSIS},
		Reason:    "Nothing is put on air until you enter your own callsign and passcode under Settings -> Integrations. The Hub will not transmit your traffic under anyone else's licence.",
	},
	{
		Feature:   "tak",
		Label:     "TAK",
		Providers: []string{integrations.ProviderTAK},
		Reason:    "Positions are not forwarded anywhere until you add your own TAK server under Settings -> Integrations.",
	},
}

// CapabilitiesHandler serves GET /api/capabilities.
type CapabilitiesHandler struct {
	svc *integrations.Service
}

// NewCapabilitiesHandler creates the handler.
func NewCapabilitiesHandler(svc *integrations.Service) *CapabilitiesHandler {
	return &CapabilitiesHandler{svc: svc}
}

// resolve fills in Configured/Platform for one feature.
//
// A lookup error is NOT treated as "unconfigured". Reporting a feature as
// unavailable because the database hiccuped would tell the customer to go and
// configure something they have already configured; the honest answer is to fail
// the request and let the SPA leave the page as it was.
func (h *CapabilitiesHandler) resolve(ctx context.Context, tenantID string, c Capability) (Capability, error) {
	for _, p := range c.Providers {
		a, err := h.svc.ForTenant(ctx, tenantID, p)
		if err != nil {
			return c, err
		}
		if a != nil {
			c.Configured = true
			c.Platform = a.Platform
			c.Reason = ""
			return c, nil
		}
	}
	return c, nil
}

// List reports, per feature, whether this tenant can actually use it.
//
//	@Summary      Which features are usable by this tenant
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {array}   Capability
//	@Failure      500  {object}  map[string]string
//	@Router       /api/capabilities [get]
func (h *CapabilitiesHandler) List(w http.ResponseWriter, r *http.Request) {
	tenantID := hubauth.TenantIDFromContext(r.Context())
	out := make([]Capability, 0, len(featureCapabilities))
	for _, c := range featureCapabilities {
		v, err := h.resolve(r.Context(), tenantID, c)
		if err != nil {
			slog.Error("capabilities: resolve", "error", err, "feature", c.Feature)
			writeError(w, http.StatusInternalServerError, "could not determine available features")
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}
