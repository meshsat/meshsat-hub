package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// variablePattern matches {word} placeholders in template bodies.
var variablePattern = regexp.MustCompile(`\{(\w+)\}`)

// MessageTemplateHandler handles message template CRUD and rendering endpoints.
type MessageTemplateHandler struct {
	store store.Store
}

// NewMessageTemplateHandler returns a new message template handler.
func NewMessageTemplateHandler(s store.Store) *MessageTemplateHandler {
	return &MessageTemplateHandler{store: s}
}

type createTemplateRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type updateTemplateRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type renderTemplateRequest struct {
	Variables map[string]string `json:"variables"`
}

type renderTemplateResponse struct {
	Text string `json:"text"`
}

// extractVariables scans a template body for {word} patterns and returns unique variable names.
func extractVariables(body string) []string {
	matches := variablePattern.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool)
	var vars []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			vars = append(vars, name)
		}
	}
	return vars
}

// ListTemplates returns all message templates for the tenant.
// @Summary List message templates
// @Tags message-templates
// @Produce json
// @Success 200 {array} store.MessageTemplate
// @Router /api/message-templates [get]
func (h *MessageTemplateHandler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	templates, err := h.store.ListMessageTemplates(r.Context(), tid)
	if err != nil {
		slog.Error("list message templates failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	if templates == nil {
		templates = []store.MessageTemplate{}
	}
	writeJSON(w, http.StatusOK, templates)
}

// CreateTemplate creates a new message template with auto-detected variables.
// @Summary Create message template
// @Tags message-templates
// @Accept json
// @Produce json
// @Param body body createTemplateRequest true "Template parameters"
// @Success 201 {object} store.MessageTemplate
// @Failure 400 {object} map[string]string
// @Router /api/message-templates [post]
func (h *MessageTemplateHandler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	var req createTemplateRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	tmpl := &store.MessageTemplate{
		ID:        fmt.Sprintf("tmpl-%d", time.Now().UnixNano()),
		Name:      req.Name,
		Body:      req.Body,
		Variables: extractVariables(req.Body),
	}

	if err := h.store.CreateMessageTemplate(r.Context(), tid, tmpl); err != nil {
		slog.Error("create message template failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create template")
		return
	}

	writeJSON(w, http.StatusCreated, tmpl)
}

// GetTemplate returns a single message template by ID.
// @Summary Get message template
// @Tags message-templates
// @Produce json
// @Param id path string true "Template ID"
// @Success 200 {object} store.MessageTemplate
// @Failure 404 {object} map[string]string
// @Router /api/message-templates/{id} [get]
func (h *MessageTemplateHandler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing template id")
		return
	}

	tmpl, err := h.store.GetMessageTemplate(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	writeJSON(w, http.StatusOK, tmpl)
}

// UpdateTemplate updates an existing message template.
// @Summary Update message template
// @Tags message-templates
// @Accept json
// @Produce json
// @Param id path string true "Template ID"
// @Param body body updateTemplateRequest true "Template parameters"
// @Success 200 {object} store.MessageTemplate
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/message-templates/{id} [put]
func (h *MessageTemplateHandler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing template id")
		return
	}

	var req updateTemplateRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := h.store.GetMessageTemplate(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Body != "" {
		existing.Body = req.Body
		existing.Variables = extractVariables(req.Body)
	}

	if err := h.store.UpdateMessageTemplate(r.Context(), tid, existing); err != nil {
		slog.Error("update message template failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update template")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// DeleteTemplate deletes a message template by ID.
// @Summary Delete message template
// @Tags message-templates
// @Param id path string true "Template ID"
// @Success 204
// @Failure 400 {object} map[string]string
// @Router /api/message-templates/{id} [delete]
func (h *MessageTemplateHandler) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing template id")
		return
	}

	if err := h.store.DeleteMessageTemplate(r.Context(), tid, id); err != nil {
		slog.Error("delete message template failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete template")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RenderTemplate renders a template with provided variable values.
// @Summary Render message template
// @Tags message-templates
// @Accept json
// @Produce json
// @Param id path string true "Template ID"
// @Param body body renderTemplateRequest true "Variable values"
// @Success 200 {object} renderTemplateResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/message-templates/{id}/render [post]
func (h *MessageTemplateHandler) RenderTemplate(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing template id")
		return
	}

	var req renderTemplateRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tmpl, err := h.store.GetMessageTemplate(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	// Replace all {variable} placeholders with provided values.
	text := tmpl.Body
	for _, v := range tmpl.Variables {
		if val, ok := req.Variables[v]; ok {
			text = strings.ReplaceAll(text, "{"+v+"}", val)
		}
	}

	writeJSON(w, http.StatusOK, renderTemplateResponse{Text: text})
}
