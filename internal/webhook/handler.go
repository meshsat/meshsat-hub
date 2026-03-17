package webhook

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// APIHandler provides REST endpoints for webhook management.
type APIHandler struct {
	dispatcher *Dispatcher
}

// NewAPIHandler creates a new webhook API handler.
func NewAPIHandler(dispatcher *Dispatcher) *APIHandler {
	return &APIHandler{dispatcher: dispatcher}
}

// ListWebhooks returns all configured webhooks (secrets redacted).
// GET /api/webhooks
func (h *APIHandler) ListWebhooks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.dispatcher.ListWebhooks())
}

// CreateWebhook adds a new webhook configuration.
// POST /api/webhooks
func (h *APIHandler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	var cfg WebhookConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if cfg.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}
	if cfg.ID == "" {
		cfg.ID = "wh-" + cfg.URL
	}
	h.dispatcher.AddWebhook(cfg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "created", "id": cfg.ID})
}

// DeleteWebhook removes a webhook by ID.
// DELETE /api/webhooks/{id}
func (h *APIHandler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, `{"error":"webhook ID required"}`, http.StatusBadRequest)
		return
	}
	h.dispatcher.RemoveWebhook(id)
	w.WriteHeader(http.StatusNoContent)
}

// GetLogs returns recent webhook delivery logs.
// GET /api/webhooks/logs
func (h *APIHandler) GetLogs(w http.ResponseWriter, _ *http.Request) {
	logs := h.dispatcher.RecentLogs(100)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}
