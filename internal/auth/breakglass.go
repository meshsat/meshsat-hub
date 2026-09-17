package auth

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// HUB_AUTH_TOKEN is a break-glass credential, and until MESHSAT-1195 its use
// left no trace at all.
//
// What it is: a single static string that yields Roles ["admin"] and
// PlatformAdmin true. "admin" ranks 3, so it satisfies RequireRole(owner), and
// the platform flag satisfies RequirePlatformAdmin. It is therefore a master
// key over every tenant, and it is accepted in EVERY auth mode -- including
// oidc, which is what production runs.
//
// Why it is still here rather than deleted. Every consumer needs the platform
// axis, not merely owner: the nightly journey suite approves a signup
// (/api/admin/signups), and the Stripe, refund and VAT tooling read
// /api/admin/*. An API key cannot carry PlatformAdmin -- deliberately, see
// main.go where the flag is left false for key-authenticated callers -- so
// "issue an API key instead" would mean weakening that guarantee to remove
// this one. That is not obviously a net gain and it is not a decision to take
// silently, so the token stays and gets the controls it never had.
//
// What changed here:
//
//   - Every acceptance is COUNTED, LOGGED at Warn and written to the AUDIT
//     CHAIN. Before this, the most privileged credential in the system was the
//     only one whose use produced no metric, no log line and no audit row. A
//     leaked token was indistinguishable from the nightly verification job.
//
//   - It is REFUSED on the onion channel. The hidden service reaches the same
//     router on its own listener, so a static master key was usable
//     anonymously over Tor -- the one path with no client identity to attribute
//     a use to, and the one where a leak is least likely to be noticed. No
//     legitimate consumer needs it there: the verification job and the operator
//     scripts all run in-cluster or from the estate.
//
// Rotation is still a redeploy and there is still no expiry. Those are real and
// tracked; this makes the credential MONITORED rather than invisible, which is
// the part that can be done without breaking the tooling that depends on it.

// BreakGlassAudit records a use of the static token. Wired in main.go to the
// audit service; nil disables the audit row (the counter and log line always
// happen).
type BreakGlassAudit func(ctx context.Context, action, actor, detail, ip string)

var breakGlassAudit BreakGlassAudit

// SetBreakGlassAudit installs the audit sink. Called once at startup.
func SetBreakGlassAudit(f BreakGlassAudit) { breakGlassAudit = f }

// breakGlassUser is the identity the static token grants.
func breakGlassUser() *User {
	return &User{ID: "token-user", Name: "API Token", Roles: []string{"admin"}, PlatformAdmin: true}
}

// tryBreakGlass reports whether the presented bearer is the static token and,
// if so, returns the context to continue with. It answers false both when the
// token does not match and when it matches but must not be honoured on this
// channel -- the caller then falls through to the normal credential checks,
// which is what makes a Tor client presenting the master key look exactly like
// a Tor client presenting nonsense.
func tryBreakGlass(w http.ResponseWriter, r *http.Request, provided, legacyToken string) (context.Context, bool) {
	if legacyToken == "" || provided == "" {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(legacyToken)) != 1 {
		return nil, false
	}

	ip := clientIPOrUnknown(r)
	ch := channelLabel(r)

	if ch == "onion" {
		// Deliberately not a distinct error: the response is the ordinary
		// invalid-credential one, so the onion cannot be used to confirm that a
		// guessed token is the real one.
		metrics.BreakGlassUseTotal.WithLabelValues("refused_onion").Inc()
		slog.Warn("auth: break-glass token presented on the onion channel and REFUSED",
			"ip", ip, "method", r.Method, "path", r.URL.Path)
		if breakGlassAudit != nil {
			breakGlassAudit(r.Context(), "breakglass_refused_onion", "token-user",
				r.Method+" "+r.URL.Path+" (onion)", ip)
		}
		return nil, false
	}

	metrics.BreakGlassUseTotal.WithLabelValues("accepted").Inc()
	slog.Warn("auth: break-glass token accepted -- platform-admin over every tenant",
		"ip", ip, "channel", ch, "method", r.Method, "path", r.URL.Path)
	if breakGlassAudit != nil {
		breakGlassAudit(r.Context(), "breakglass_used", "token-user",
			r.Method+" "+r.URL.Path, ip)
	}
	return context.WithValue(r.Context(), UserContextKey, breakGlassUser()), true
}
