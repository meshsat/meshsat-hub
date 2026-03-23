package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// AuditHandler handles audit log API endpoints.
type AuditHandler struct {
	audit *audit.Service
}

// NewAuditHandler returns a new audit handler.
func NewAuditHandler(a *audit.Service) *AuditHandler {
	return &AuditHandler{audit: a}
}

// ListEntries returns audit log entries for the tenant.
// @Summary List audit log entries
// @Tags audit
// @Produce json
// @Param limit query int false "Max results" default(100)
// @Success 200 {array} store.AuditEntry
// @Router /api/audit [get]
func (h *AuditHandler) ListEntries(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	entries, err := h.audit.Store().ListAuditEntries(r.Context(), tid, limit)
	if err != nil {
		slog.Error("list audit entries", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list audit entries")
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}

	if r.URL.Query().Get("format") == "csv" {
		rows := make([][]string, len(entries))
		for i, e := range entries {
			rows[i] = []string{
				e.CreatedAt.Format(time.RFC3339),
				e.Action,
				e.Actor,
				e.Detail,
			}
		}
		writeCSV(w, "audit.csv", []string{"timestamp", "action", "actor", "detail"}, rows)
		return
	}

	writeJSON(w, http.StatusOK, entries)
}

// VerifyChain verifies the integrity of the audit hash chain.
// @Summary Verify audit chain integrity
// @Tags audit
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /api/audit/verify [get]
func (h *AuditHandler) VerifyChain(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	verified, broken, err := h.audit.VerifyChain(r.Context(), tid)
	if err != nil {
		slog.Error("verify audit chain", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to verify chain")
		return
	}

	result := map[string]interface{}{
		"verified": verified,
		"valid":    broken == nil,
	}
	if broken != nil {
		result["broken_at"] = broken
	}
	writeJSON(w, http.StatusOK, result)
}
