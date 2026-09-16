package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// Observability for refused requests (MESHSAT-1190).
//
// A 401 from this package used to produce nothing at all: no metric, no log
// line, no audit row. The cause is structural rather than an oversight --
// hubauth.Middleware is registered in main.go OUTSIDE both
// metrics.ChiMiddleware and hubmw.Logging, because the logger is deliberately
// innermost so it can see the authenticated user and tenant. An outer
// middleware cannot see those (auth attaches them to a new context that only
// the inner chain holds), so moving the logger out would trade this blind spot
// for a different one. The refusal has to report itself, at the point it
// happens, where the reason is actually known.
//
// That turns out to be better than the generic log line would have been: the
// reason is a stable label, so "somebody is trying tokens" and "somebody is
// trying API keys" are distinguishable, and a spike in 403s (already inside,
// reaching further) is separable from a spike in 401s (trying to get in).

// Stable metric labels for a refusal. The client-facing message is derived
// from these, so the wire text and the label cannot drift apart.
const (
	denyMissingCredential = "missing_credential"
	denyInvalidToken      = "invalid_token"
	denyInvalidClaims     = "invalid_claims"
	denyUnknownSubject    = "unknown_subject"
	denyInvalidAPIKey     = "invalid_api_key"
	denyAPIKeyExpired     = "api_key_expired"

	denyTenantRequired  = "tenant_required"
	denyTenantSuspended = "tenant_suspended"
	denyTenantDeleted   = "tenant_deleted"
)

// denyMessage maps a reason to the text the client sees. The messages are
// deliberately vague and deliberately unchanged from before this file existed:
// a caller learns only that it was refused, never which of the checks refused
// it. The precision lives in the log line and the metric, which are ours.
var denyMessage = map[string]string{ // #nosec G101 -- refusal messages shown to a rejected caller, not credentials; the words "token" and "API key" are what tripped the rule
	denyMissingCredential: "missing Authorization header",
	denyInvalidToken:      "invalid token",
	denyInvalidClaims:     "invalid token claims",
	denyUnknownSubject:    "unknown subject",
	denyInvalidAPIKey:     "invalid API key",
	denyAPIKeyExpired:     "API key expired",

	denyTenantRequired:  "tenant context required",
	denyTenantSuspended: "tenant suspended",
	denyTenantDeleted:   "tenant deleted",
}

// clientIPCtxKey carries the rate-limit-safe client address computed by
// middleware.ClientIPContext.
//
// It lives in THIS package because internal/middleware imports internal/auth,
// so the dependency can only point this way. auth cannot call
// middleware.ClientIP itself, and duplicating that function here is exactly
// how the two wrong answers it documents came to exist in the first place --
// so the one correct implementation stays in middleware and hands its answer
// over through the context.
type clientIPCtxKey struct{}

// WithClientIP stores the client address for this request. Called once, by
// middleware.ClientIPContext, as early in the chain as possible.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPCtxKey{}, ip)
}

// ClientIPFromContext returns the address stored by WithClientIP, or "" when
// the middleware did not run. "" is reported as "unknown" rather than
// substituted with RemoteAddr: naming the ingress pod as the actor is what
// made every existing HTTP log line useless.
func ClientIPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// channelLabel reports which listener accepted the request. The channel is
// established by the listener, never by a header, so it cannot be forged.
// internal/middleware owns the marker for the same import-direction reason as
// the client IP, so this reads the value it stored.
type channelCtxKey struct{}

// WithChannelLabel records the arrival channel ("internet" or "onion").
func WithChannelLabel(ctx context.Context, channel string) context.Context {
	return context.WithValue(ctx, channelCtxKey{}, channel)
}

func channelLabel(r *http.Request) string {
	if v, ok := r.Context().Value(channelCtxKey{}).(string); ok && v != "" {
		return v
	}
	// Unmarked means the internet edge, matching middleware.ChannelOf's own
	// default: it is the path whose forwarding headers our proxies set, so an
	// unmarked request can never inherit the onion's treatment by accident.
	return "internet"
}

func clientIPOrUnknown(r *http.Request) string {
	if ip := ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return "unknown"
}

// writeAuthError refuses a request for want of a valid credential, and says so
// where somebody can see it.
func writeAuthError(w http.ResponseWriter, r *http.Request, reason string) {
	msg, ok := denyMessage[reason]
	if !ok {
		msg = "unauthorized"
	}
	ch := channelLabel(r)
	metrics.AuthFailuresTotal.WithLabelValues(reason, ch).Inc()
	// Warn, not Debug: production runs at info, and a refused credential is
	// the single most useful security signal this service can emit.
	slog.Warn("auth: request refused",
		"reason", reason,
		"channel", ch,
		"ip", clientIPOrUnknown(r),
		"method", r.Method,
		"path", r.URL.Path,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	writeJSONError(w, msg)
}

// writeAuthzDenial refuses a request that carried a valid credential but not
// enough authority.
func writeAuthzDenial(w http.ResponseWriter, r *http.Request, requirement, msg string) {
	ch := channelLabel(r)
	metrics.AuthzDenialsTotal.WithLabelValues(requirement, ch).Inc()
	actor, tenant := "", ""
	if u := FromContext(r.Context()); u != nil {
		actor = u.ID
	}
	if t, ok := r.Context().Value(TenantContextKey).(string); ok {
		tenant = t
	}
	slog.Warn("auth: request denied",
		"requirement", requirement,
		"channel", ch,
		"ip", clientIPOrUnknown(r),
		"actor", actor,
		"tenant", tenant,
		"method", r.Method,
		"path", r.URL.Path,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	writeJSONError(w, msg)
}

// writeTenantBlocked refuses a request whose tenant may not be served.
func writeTenantBlocked(w http.ResponseWriter, r *http.Request, reason string) {
	msg, ok := denyMessage[reason]
	if !ok {
		msg = "forbidden"
	}
	writeAuthzDenial(w, r, reason, msg)
}

// writeJSONError writes the refusal body. %q rather than "%s": the original
// writeAuthError interpolated the message raw, which would emit broken JSON
// for any message containing a quote. Every message here is a constant today,
// so this is closing the shape rather than a live bug.
func writeJSONError(w http.ResponseWriter, msg string) {
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}
