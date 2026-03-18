package api

import (
	"net/http"
	"strconv"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// MessageHandler provides REST endpoints for message history.
type MessageHandler struct {
	store store.Store
}

// NewMessageHandler creates a new message API handler.
func NewMessageHandler(s store.Store) *MessageHandler {
	return &MessageHandler{store: s}
}

// ListMessages returns messages, optionally filtered by device IMEI.
// @Summary List messages
// @Tags messages
// @Produce json
// @Param device query string false "Filter by device IMEI"
// @Param limit query int false "Max results" default(100)
// @Success 200 {array} store.Message
// @Failure 500 {object} map[string]string
// @Router /api/messages [get]
func (h *MessageHandler) ListMessages(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	tid := auth.TenantIDFromContext(r.Context())
	msgs, err := h.store.ListMessages(r.Context(), tid, device, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// GetMessage returns a single message by ID.
// @Summary Get message by ID
// @Tags messages
// @Produce json
// @Param id path string true "Message ID"
// @Success 200 {object} store.Message
// @Failure 404 {object} map[string]string
// @Router /api/messages/{id} [get]
func (h *MessageHandler) GetMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	msg, err := h.store.GetMessage(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	writeJSON(w, http.StatusOK, msg)
}
