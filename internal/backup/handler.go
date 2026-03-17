package backup

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// APIHandler provides REST endpoints for backup/restore.
type APIHandler struct {
	provider StateProvider
	dataDir  string
}

// NewAPIHandler creates a new backup API handler.
func NewAPIHandler(provider StateProvider, dataDir string) *APIHandler {
	return &APIHandler{provider: provider, dataDir: dataDir}
}

// ExportBackup creates and returns a ZIP backup.
// GET /api/backup/export
func (h *APIHandler) ExportBackup(w http.ResponseWriter, _ *http.Request) {
	data, err := Export(h.provider, h.dataDir)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	filename := "meshsat-hub-backup-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write(data)
}

// DiffBackup parses an uploaded backup ZIP and returns what would change.
// POST /api/backup/diff
func (h *APIHandler) DiffBackup(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 50*1024*1024)) // 50MB max
	if err != nil {
		http.Error(w, `{"error":"read body: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	snap, err := ParseManifest(data)
	if err != nil {
		http.Error(w, `{"error":"parse backup: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	diff, err := Diff(h.provider, snap, h.dataDir)
	if err != nil {
		http.Error(w, `{"error":"diff: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(diff)
}

// ImportBackup restores data files from an uploaded backup ZIP.
// POST /api/backup/import
func (h *APIHandler) ImportBackup(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 50*1024*1024))
	if err != nil {
		http.Error(w, `{"error":"read body: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	snap, err := ParseManifest(data)
	if err != nil {
		http.Error(w, `{"error":"parse backup: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	if err := Import(data, h.dataDir); err != nil {
		http.Error(w, `{"error":"import: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "imported",
		"created_at": snap.CreatedAt,
		"data_files": len(snap.DataFiles),
	})
}
