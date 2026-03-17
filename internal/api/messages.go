package api

import (
	"net/http"
	"strconv"

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
// GET /api/messages?device={imei}&limit={n}
func (h *MessageHandler) ListMessages(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	msgs, err := h.store.ListMessages(r.Context(), device, limit)
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
// GET /api/messages/{id}
func (h *MessageHandler) GetMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	msg, err := h.store.GetMessage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	writeJSON(w, http.StatusOK, msg)
}
