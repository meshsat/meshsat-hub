package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/integrations"
)

// TenantIntegrationsHandler exposes a tenant's provider accounts
// (MESHSAT-977): GET/PUT/DELETE /api/tenant/integrations/{provider} and a
// connectivity test. Secrets are write-only.
type TenantIntegrationsHandler struct {
	svc   *integrations.Service
	audit *audit.Service
}

// NewTenantIntegrationsHandler creates the handler.
func NewTenantIntegrationsHandler(svc *integrations.Service, auditSvc *audit.Service) *TenantIntegrationsHandler {
	return &TenantIntegrationsHandler{svc: svc, audit: auditSvc}
}

type providerAccountView struct {
	integrations.Spec
	Configured  bool              `json:"configured"`
	Platform    bool              `json:"platform"` // the default tenant's environment account
	Fields      map[string]string `json:"values"`   // masked
	UpdatedAt   *time.Time        `json:"updated_at,omitempty"`
	WebhookPath string            `json:"webhook_path,omitempty"` // relative; the SPA prefixes its origin
}

func (h *TenantIntegrationsHandler) view(ctx context.Context, tenantID string, spec integrations.Spec) (providerAccountView, error) {
	v := providerAccountView{Spec: spec, Fields: map[string]string{}}
	a, err := h.svc.ForTenant(ctx, tenantID, spec.Provider)
	if err != nil {
		return v, err
	}
	if a != nil {
		v.Configured = true
		v.Platform = a.Platform
		v.Fields = integrations.Masked(spec, a)
		if !a.UpdatedAt.IsZero() {
			t := a.UpdatedAt
			v.UpdatedAt = &t
		}
	} else {
		for _, f := range spec.Fields {
			v.Fields[f.Key] = ""
		}
	}
	if spec.Webhook != "" {
		v.WebhookPath = spec.Webhook
		if a != nil && spec.Provider == integrations.ProviderCloudloop && a.Get("webhook_token") != "" {
			v.WebhookPath += "?token=" + integrations.Masked(spec, a)["webhook_token"]
		}
	}
	return v, nil
}

// List returns every provider with the tenant's masked account.
//
//	@Summary      List the tenant's provider accounts
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {array}   providerAccountView
//	@Router       /api/tenant/integrations [get]
func (h *TenantIntegrationsHandler) List(w http.ResponseWriter, r *http.Request) {
	tenantID := hubauth.TenantIDFromContext(r.Context())
	out := make([]providerAccountView, 0, len(integrations.Specs))
	for _, spec := range integrations.Specs {
		v, err := h.view(r.Context(), tenantID, spec)
		if err != nil {
			slog.Error("integrations: list", "error", err, "provider", spec.Provider)
			writeError(w, http.StatusInternalServerError, "could not load provider accounts")
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type putProviderAccountRequest struct {
	Values map[string]string `json:"values"`
}

// Put creates or updates the tenant's account for a provider. Empty secret
// values keep the stored secret; generated tokens are returned once in
// "reveal" so the tenant can paste them into the provider's console.
//
//	@Summary      Set the tenant's provider account
//	@Tags         tenant
//	@Accept       json
//	@Produce      json
//	@Param        provider  path  string  true  "cloudloop|twilio|rock7|rockblock|globalstar"
//	@Param        body  body  putProviderAccountRequest  true  "field values"
//	@Success      200  {object}  map[string]interface{}
//	@Failure      400  {object}  map[string]string
//	@Router       /api/tenant/integrations/{provider} [put]
func (h *TenantIntegrationsHandler) Put(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	spec, ok := integrations.SpecFor(provider)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	var req putProviderAccountRequest
	if err := readJSON(w, r, &req, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenantID := hubauth.TenantIDFromContext(r.Context())
	acct, err := h.svc.Set(r.Context(), tenantID, provider, req.Values)
	if err != nil {
		if errors.Is(err, integrations.ErrUnknownProvider) {
			writeError(w, http.StatusNotFound, "unknown provider")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.log(r, "integration_updated", provider)
	v, _ := h.view(r.Context(), tenantID, spec)
	reveal := map[string]string{}
	for _, f := range spec.Fields {
		if f.Generate {
			reveal[f.Key] = acct.Get(f.Key)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": v, "reveal": reveal})
}

// Delete removes the tenant's account for a provider (the default tenant
// falls back to the platform account).
//
//	@Summary      Delete the tenant's provider account
//	@Tags         tenant
//	@Param        provider  path  string  true  "provider"
//	@Success      204
//	@Router       /api/tenant/integrations/{provider} [delete]
func (h *TenantIntegrationsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if _, ok := integrations.SpecFor(provider); !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	tenantID := hubauth.TenantIDFromContext(r.Context())
	if err := h.svc.Delete(r.Context(), tenantID, provider); err != nil {
		slog.Error("integrations: delete", "error", err, "provider", provider)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	h.log(r, "integration_deleted", provider)
	w.WriteHeader(http.StatusNoContent)
}

// Test checks the stored account against the provider.
//
//	@Summary      Test the tenant's provider account
//	@Tags         tenant
//	@Produce      json
//	@Param        provider  path  string  true  "provider"
//	@Success      200  {object}  map[string]interface{}
//	@Failure      400  {object}  map[string]string
//	@Router       /api/tenant/integrations/{provider}/test [post]
func (h *TenantIntegrationsHandler) Test(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if _, ok := integrations.SpecFor(provider); !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	tenantID := hubauth.TenantIDFromContext(r.Context())
	acct, err := h.svc.Require(r.Context(), tenantID, provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch provider {
	case integrations.ProviderCloudloop:
		bal, err := cloudloop.NewClient(acct.Get("api_url"), acct.Get("api_key")).GetCreditBalance(ctx)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": fmt.Sprintf("credit balance %v", bal.Balance)})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "no connectivity test for this provider yet"})
	}
}

func (h *TenantIntegrationsHandler) log(r *http.Request, action, provider string) {
	if h.audit == nil {
		return
	}
	actor := "unknown"
	if u := hubauth.FromContext(r.Context()); u != nil {
		actor = u.Email
	}
	if err := h.audit.Log(r.Context(), hubauth.TenantIDFromContext(r.Context()), action, actor, "provider="+provider, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: integration event", "error", err)
	}
}
