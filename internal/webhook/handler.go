package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
)

// APIHandler provides REST endpoints for webhook management.
type APIHandler struct {
	dispatcher *Dispatcher
}

// NewAPIHandler creates a new webhook API handler.
func NewAPIHandler(dispatcher *Dispatcher) *APIHandler {
	return &APIHandler{dispatcher: dispatcher}
}

// tenantOf returns the caller's tenant.
//
// auth.TenantIDFromContext never returns "": with no tenant on the context it
// answers "default", which is single-tenant compatibility and is also the
// platform tenant. So this is only correct as long as these routes stay behind
// auth.TenantMiddleware, which is what puts the real tenant there. A route
// registered outside it would write every customer's webhooks into the platform
// tenant -- exactly the shape of the dead man's switch bug in the same issue.
func tenantOf(r *http.Request) string {
	return auth.TenantIDFromContext(r.Context())
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%s}`, strconv.Quote(msg))
}

// decodeBody applies the three guards internal/api.readJSON applies -- a body
// size limit, unknown fields refused, a single JSON value -- as critical rule 4
// requires.
//
// It is a local copy rather than an import because readJSON is unexported, and
// reaching into internal/api from here would pull the entire handler set into
// internal/routing, which imports this package and carries field traffic. That
// is exactly the coupling MESHSAT-992 records.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	const maxBody = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON object")
	}
	return nil
}

// ListWebhooks returns the caller's own webhooks (secrets redacted).
// @Summary List outbound webhooks
// @Tags webhooks
// @Produce json
// @Success 200 {array} WebhookConfig
// @Router /api/webhooks [get]
func (h *APIHandler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantOf(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.dispatcher.ListWebhooks(tenantID))
}

// CreateWebhook adds a new webhook configuration for the caller's tenant.
// @Summary Create outbound webhook
// @Tags webhooks
// @Accept json
// @Produce json
// @Param body body WebhookConfig true "Webhook configuration (url required)"
// @Success 201 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/webhooks [post]
func (h *APIHandler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantOf(r)
	var cfg WebhookConfig
	if err := decodeBody(w, r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The session decides the owner, never the body. A tenant_id sent by the
	// caller is overwritten here, unconditionally: it is the one field that
	// would otherwise let a customer subscribe to another tenant's traffic.
	cfg.TenantID = tenantID

	if cfg.URL == "" {
		writeErr(w, http.StatusBadRequest, "url is required")
		return
	}
	// The Hub fetches this URL with its own network identity from inside the
	// cluster, so a target on our side of the wire is a request-forgery
	// primitive rather than a webhook. Refused here and again at delivery --
	// through the dispatcher's own checkTarget, so registration and delivery
	// are literally the same policy rather than two copies that can drift.
	if err := h.dispatcher.checkTarget(cfg.URL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if cfg.ID == "" {
		// Namespaced by tenant: the id is the primary key of webhook_configs,
		// so two tenants registering the same URL used to collide -- and with
		// ON CONFLICT DO UPDATE, the second would have taken over the first's
		// row.
		cfg.ID = "wh-" + tenantID + "-" + cfg.URL
	}
	if err := h.dispatcher.Save(r.Context(), cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the webhook")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "created", "id": cfg.ID})
}

// DeleteWebhook removes one of the caller's webhooks by ID.
// @Summary Delete outbound webhook
// @Tags webhooks
// @Param id path string true "Webhook ID"
// @Success 204
// @Failure 400 {object} map[string]string
// @Router /api/webhooks/{id} [delete]
func (h *APIHandler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantOf(r)
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "webhook ID required")
		return
	}
	if _, err := h.dispatcher.Delete(r.Context(), tenantID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not delete the webhook")
		return
	}
	// 204 whether or not a row was there. The delete is scoped to the caller's
	// tenant either way, and distinguishing "yours, gone" from "not yours"
	// would answer whether another tenant holds that id.
	w.WriteHeader(http.StatusNoContent)
}

// GetLogs returns the caller's recent webhook delivery logs.
// @Summary Get recent webhook delivery logs
// @Tags webhooks
// @Produce json
// @Success 200 {array} DeliveryLog
// @Router /api/webhooks/logs [get]
func (h *APIHandler) GetLogs(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantOf(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.dispatcher.RecentLogs(tenantID, 100))
}
