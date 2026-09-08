// Package webhookroute resolves the tenant that owns an inbound provider
// webhook from a secret in the request path (MESHSAT-975).
//
// Providers post to one URL per provider and know nothing about tenants, so
// something has to decide whose data an inbound message is. Passing the tenant
// as an optional query parameter left a fallback for every case it was absent,
// and the fallback guessed: first from the device, then from the default
// tenant. Guessing wrong writes one customer's traffic into another's tables.
//
// Giving each tenant its own path removes the question. The route does not
// exist without a secret, an unknown secret is a 404, and a handler behind this
// middleware always has exactly one tenant. There is nothing left to fall back
// to and therefore nothing to get wrong.
//
// The secret identifies; it does not authenticate. A URL travels through
// consoles, logs and support tickets, so the provider's own signature check
// still runs behind this, and where a provider cannot sign, its published
// egress range is the compensating control. See internal/rockblock and
// internal/globalstar for both shapes.
package webhookroute

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// URLParam is the chi path parameter carrying the per-tenant secret.
const URLParam = "secret"

type ctxKey struct{}

// Resolved is the outcome of matching a path secret to a provider account.
type Resolved struct {
	// TenantID owns the account the secret belongs to. Never empty.
	TenantID string
	// Account is that tenant's provider account, or the platform account when
	// the secret is the platform one. Carries the signing secret a handler
	// needs to verify the provider's signature.
	Account *integrations.Account
}

// FromContext returns the tenant resolved for this request.
func FromContext(ctx context.Context) (*Resolved, bool) {
	v, ok := ctx.Value(ctxKey{}).(*Resolved)
	return v, ok
}

// TenantID returns the resolved tenant, or "" when the request did not come
// through this middleware. A handler that gets "" must refuse rather than
// choose a tenant of its own.
func TenantID(ctx context.Context) string {
	if r, ok := FromContext(ctx); ok {
		return r.TenantID
	}
	return ""
}

// Middleware resolves the {secret} path parameter into the tenant whose
// provider account carries it in fieldKey, and puts both on the context.
//
// An unmatched secret is answered 404, not 401: a 401 would confirm that the
// path shape is right and turn the endpoint into an oracle for guessing
// secrets. A 404 is what any other unknown URL returns.
func Middleware(svc *integrations.Service, provider, fieldKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if svc == nil {
				http.NotFound(w, r)
				return
			}
			secret := chi.URLParam(r, URLParam)
			if secret == "" {
				http.NotFound(w, r)
				return
			}
			tenantID, account, err := svc.LookupByToken(r.Context(), provider, fieldKey, secret)
			if err != nil {
				slog.Error("webhook: tenant lookup failed", "provider", provider, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if tenantID == "" {
				slog.Warn("webhook: unknown path secret", "provider", provider, "ip", clientIP(r))
				http.NotFound(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, &Resolved{TenantID: tenantID, Account: account})
			ctx = tenancy.WithTenant(ctx, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// clientIP reports the caller for the warning above, honouring the edge relay's
// forwarded header the same way the API handlers do.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return r.RemoteAddr
}
