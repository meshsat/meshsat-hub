package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot represents the full exportable state of a Hub instance.
type Snapshot struct {
	Version   string          `json:"version"`
	CreatedAt string          `json:"created_at"`
	Config    json.RawMessage `json:"config"`
	Webhooks  json.RawMessage `json:"webhooks"`
	DataFiles []DataFile      `json:"data_files,omitempty"`
}

// DataFile is a file included in the backup archive.
type DataFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// DiffResult describes what would change if a snapshot were imported.
type DiffResult struct {
	ConfigChanged   bool     `json:"config_changed"`
	WebhooksChanged bool     `json:"webhooks_changed"`
	FilesAdded      []string `json:"files_added,omitempty"`
	FilesModified   []string `json:"files_modified,omitempty"`
	FilesRemoved    []string `json:"files_removed,omitempty"`
}

// StateProvider supplies the current Hub state for export.
type StateProvider interface {
	ExportConfig() (json.RawMessage, error)
	ExportWebhooks() (json.RawMessage, error)
}

// Export creates a ZIP archive containing the full Hub state.
// The archive contains:
// - manifest.json: snapshot metadata + inline config/webhooks
// - data/*: files from the data directory (SQLite DB, etc.)
func Export(provider StateProvider, dataDir string) ([]byte, error) {
	configJSON, err := provider.ExportConfig()
	if err != nil {
		return nil, fmt.Errorf("export config: %w", err)
	}

	webhooksJSON, err := provider.ExportWebhooks()
	if err != nil {
		return nil, fmt.Errorf("export webhooks: %w", err)
	}

	snap := Snapshot{
		Version:   "1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Config:    configJSON,
		Webhooks:  webhooksJSON,
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Add data directory files
	if dataDir != "" {
		err := filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			relPath, _ := filepath.Rel(dataDir, path)
			snap.DataFiles = append(snap.DataFiles, DataFile{
				Path: relPath,
				Size: info.Size(),
			})

			fw, err := zw.Create("data/" + relPath)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(fw, f)
			return err
		})
		if err != nil {
			slog.Warn("backup: data dir walk error", "error", err, "dir", dataDir)
		}
	}

	// Write manifest
	manifestData, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	mw, err := zw.Create("manifest.json")
	if err != nil {
		return nil, err
	}
	if _, err := mw.Write(manifestData); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}

	slog.Info("backup: export complete", "size", buf.Len(), "data_files", len(snap.DataFiles))
	return buf.Bytes(), nil
}

// ParseManifest reads the manifest from a backup ZIP archive.
func ParseManifest(zipData []byte) (*Snapshot, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}

	for _, f := range r.File {
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			var snap Snapshot
			if err := json.NewDecoder(rc).Decode(&snap); err != nil {
				return nil, fmt.Errorf("parse manifest: %w", err)
			}
			return &snap, nil
		}
	}
	return nil, fmt.Errorf("manifest.json not found in archive")
}

// Diff compares a backup snapshot against the current state without applying changes.
func Diff(provider StateProvider, incoming *Snapshot, dataDir string) (*DiffResult, error) {
	result := &DiffResult{}

	// Compare config
	currentConfig, err := provider.ExportConfig()
	if err != nil {
		return nil, err
	}
	result.ConfigChanged = !jsonEqual(currentConfig, incoming.Config)

	// Compare webhooks
	currentWebhooks, err := provider.ExportWebhooks()
	if err != nil {
		return nil, err
	}
	result.WebhooksChanged = !jsonEqual(currentWebhooks, incoming.Webhooks)

	// Compare data files
	if dataDir != "" {
		currentFiles := make(map[string]int64)
		_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dataDir, path)
			currentFiles[rel] = info.Size()
			return nil
		})

		incomingFiles := make(map[string]int64)
		for _, df := range incoming.DataFiles {
			incomingFiles[df.Path] = df.Size
		}

		for path, size := range incomingFiles {
			curSize, exists := currentFiles[path]
			if !exists {
				result.FilesAdded = append(result.FilesAdded, path)
			} else if curSize != size {
				result.FilesModified = append(result.FilesModified, path)
			}
		}
		for path := range currentFiles {
			if _, exists := incomingFiles[path]; !exists {
				result.FilesRemoved = append(result.FilesRemoved, path)
			}
		}
	}

	return result, nil
}

// Import restores data files from a backup ZIP into the data directory.
// Config and webhook restoration must be handled by the caller via the StateProvider.
func Import(zipData []byte, dataDir string) error {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	for _, f := range r.File {
		if f.Name == "manifest.json" {
			continue
		}
		// Only extract data/ files
		if len(f.Name) > 5 && f.Name[:5] == "data/" {
			relPath := f.Name[5:]
			// Path traversal protection: ensure destPath stays within dataDir
			destPath := filepath.Join(dataDir, relPath)
			if !strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(dataDir)+string(os.PathSeparator)) {
				slog.Warn("backup: path traversal blocked", "path", relPath)
				continue
			}

			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(destPath), err)
			}

			rc, err := f.Open()
			if err != nil {
				return err
			}

			out, err := os.Create(destPath)
			if err != nil {
				_ = rc.Close()
				return fmt.Errorf("create %s: %w", destPath, err)
			}

			_, err = io.Copy(out, rc)
			_ = rc.Close()
			_ = out.Close()
			if err != nil {
				return fmt.Errorf("write %s: %w", destPath, err)
			}
			slog.Info("backup: restored file", "path", relPath)
		}
	}

	return nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var va, vb interface{}
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return bytes.Equal(ja, jb)
}
