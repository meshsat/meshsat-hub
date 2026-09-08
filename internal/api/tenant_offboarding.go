package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// TenantOffboardingHandler serves the two halves of the same promise: a tenant
// can take its data with it, and it can have it destroyed. Deletion is soft
// and reversible for store.PurgeGrace; the purge job finishes the job.
type TenantOffboardingHandler struct {
	store  store.Store
	audit  *audit.Service
	forget func(tenantID string) // drops the cached status so a delete applies now
}

// NewTenantOffboardingHandler creates the handler. forget may be nil.
func NewTenantOffboardingHandler(s store.Store, a *audit.Service, forget func(string)) *TenantOffboardingHandler {
	return &TenantOffboardingHandler{store: s, audit: a, forget: forget}
}

// Export streams every row the tenant owns as a ZIP of JSON files, one per
// table, plus a manifest.
//
//	@Summary      Export everything this tenant owns
//	@Description  A ZIP with one JSON file per table and a manifest. Owner only.
//	@Tags         tenant
//	@Produce      application/zip
//	@Success      200  {file}    binary
//	@Failure      403  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/tenant/export [get]
func (h *TenantOffboardingHandler) Export(w http.ResponseWriter, r *http.Request) {
	tenantID := hubauth.TenantIDFromContext(r.Context())
	data, err := h.store.ExportTenant(r.Context(), tenantID)
	if err != nil {
		slog.Error("tenant export failed", "tenant", tenantID, "error", err)
		writeError(w, http.StatusInternalServerError, "export failed")
		return
	}
	rows := 0
	for _, v := range data {
		rows += len(v)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "meshsat-"+tenantID+"-"+time.Now().UTC().Format("20060102")+".zip"))

	z := zip.NewWriter(w)
	manifest := map[string]any{
		"tenant_id":  tenantID,
		"exported":   time.Now().UTC().Format(time.RFC3339),
		"tables":     len(data),
		"rows":       rows,
		"note":       "One JSON file per table. Every row here carries this tenant's id; nothing from another tenant is included.",
		"row_counts": map[string]int{},
	}
	for t, v := range data {
		manifest["row_counts"].(map[string]int)[t] = len(v)
	}
	if err := writeZipJSON(z, "manifest.json", manifest); err != nil {
		slog.Error("tenant export: manifest", "error", err)
		return
	}
	for table, recs := range data {
		if err := writeZipJSON(z, table+".json", recs); err != nil {
			slog.Error("tenant export: table", "table", table, "error", err)
			return
		}
	}
	if err := z.Close(); err != nil {
		slog.Error("tenant export: close", "error", err)
		return
	}
	h.logAudit(r, tenantID, "tenant_exported", fmt.Sprintf("tables=%d rows=%d", len(data), rows))
}

func writeZipJSON(z *zip.Writer, name string, v any) error {
	f, err := z.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Delete blocks the tenant now and schedules the destruction of its data.
//
//	@Summary      Close this tenant
//	@Description  Blocks access immediately and destroys the data after the grace period. Owner only. Reversible until then.
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {object}  map[string]interface{}
//	@Failure      403  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/tenant [delete]
func (h *TenantOffboardingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID := hubauth.TenantIDFromContext(r.Context())
	h.softDelete(w, r, tenantID)
}

// AdminDelete is the platform-admin equivalent, addressed by id.
//
//	@Summary      Close a tenant
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      string  true  "tenant id"
//	@Success      200  {object}  map[string]interface{}
//	@Failure      403  {object}  map[string]string
//	@Router       /api/admin/tenants/{id} [delete]
func (h *TenantOffboardingHandler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	h.softDelete(w, r, chi.URLParam(r, "id"))
}

func (h *TenantOffboardingHandler) softDelete(w http.ResponseWriter, r *http.Request, tenantID string) {
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant required")
		return
	}
	// The platform tenant owns the deployment; closing it would lock everyone
	// out of their own Hub.
	if tenantID == store.DefaultTenantID {
		writeError(w, http.StatusForbidden, "the platform tenant cannot be closed")
		return
	}
	at := time.Now().UTC()
	if err := h.store.SoftDeleteTenant(r.Context(), tenantID, at); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "unknown tenant")
			return
		}
		slog.Error("tenant delete failed", "tenant", tenantID, "error", err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if h.forget != nil {
		h.forget(tenantID) // so the block applies to the next request, not the next TTL
	}
	purgeAt := at.Add(store.PurgeGrace)
	h.logAudit(r, tenantID, "tenant_deleted", "purge_at="+purgeAt.Format(time.RFC3339))
	slog.Warn("tenant closed", "tenant", tenantID, "purge_at", purgeAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":  tenantID,
		"status":     store.TenantDeleted,
		"deleted_at": at.Format(time.RFC3339),
		"purge_at":   purgeAt.Format(time.RFC3339),
		"reversible": "until purge_at, by setting the tenant status back to active",
	})
}

func (h *TenantOffboardingHandler) logAudit(r *http.Request, tenantID, action, detail string) {
	if h.audit == nil {
		return
	}
	actor := "unknown"
	if u := hubauth.FromContext(r.Context()); u != nil && u.Email != "" {
		actor = u.Email
	}
	if err := h.audit.Log(r.Context(), tenantID, action, actor, detail, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: failed to log "+action, "error", err)
	}
}
