package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mockProvider implements StateProvider for tests.
type mockProvider struct {
	config   json.RawMessage
	webhooks json.RawMessage
}

func (m *mockProvider) ExportConfig() (json.RawMessage, error) {
	return m.config, nil
}
func (m *mockProvider) ExportWebhooks() (json.RawMessage, error) {
	return m.webhooks, nil
}

func TestExportAndParseManifest(t *testing.T) {
	provider := &mockProvider{
		config:   json.RawMessage(`{"port":6070,"mqtt":"tcp://mqtt:1883"}`),
		webhooks: json.RawMessage(`[{"id":"wh-1","url":"http://example.com"}]`),
	}

	zipData, err := Export(provider, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(zipData) == 0 {
		t.Fatal("empty zip")
	}

	snap, err := ParseManifest(zipData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.Version != "1" {
		t.Errorf("version: %q", snap.Version)
	}
	if snap.CreatedAt == "" {
		t.Error("missing created_at")
	}

	// Verify config round-trip
	var cfg map[string]interface{}
	if err := json.Unmarshal(snap.Config, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg["port"].(float64) != 6070 {
		t.Errorf("config port: %v", cfg["port"])
	}
}

func TestExportWithDataFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "test.db"), []byte("fake database"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "subdir", "config.yaml"), []byte("key: value"), 0o644)

	provider := &mockProvider{
		config:   json.RawMessage(`{}`),
		webhooks: json.RawMessage(`[]`),
	}

	zipData, err := Export(provider, dir)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	snap, err := ParseManifest(zipData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.DataFiles) != 2 {
		t.Fatalf("expected 2 data files, got %d", len(snap.DataFiles))
	}
}

func TestImport(t *testing.T) {
	// Create a backup from source
	srcDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcDir, "test.db"), []byte("database contents"), 0o644)

	provider := &mockProvider{
		config:   json.RawMessage(`{"port":6070}`),
		webhooks: json.RawMessage(`[]`),
	}

	zipData, err := Export(provider, srcDir)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// Import into a new directory
	destDir := t.TempDir()
	if err := Import(zipData, destDir); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Verify file was restored
	data, err := os.ReadFile(filepath.Join(destDir, "test.db"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(data) != "database contents" {
		t.Errorf("restored content: %q", string(data))
	}
}

func TestDiff_NoChanges(t *testing.T) {
	provider := &mockProvider{
		config:   json.RawMessage(`{"port":6070}`),
		webhooks: json.RawMessage(`[]`),
	}

	snap := &Snapshot{
		Config:   json.RawMessage(`{"port":6070}`),
		Webhooks: json.RawMessage(`[]`),
	}

	diff, err := Diff(provider, snap, "")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if diff.ConfigChanged {
		t.Error("config should not be changed")
	}
	if diff.WebhooksChanged {
		t.Error("webhooks should not be changed")
	}
}

func TestDiff_ConfigChanged(t *testing.T) {
	provider := &mockProvider{
		config:   json.RawMessage(`{"port":6070}`),
		webhooks: json.RawMessage(`[]`),
	}

	snap := &Snapshot{
		Config:   json.RawMessage(`{"port":8080}`), // different port
		Webhooks: json.RawMessage(`[]`),
	}

	diff, err := Diff(provider, snap, "")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !diff.ConfigChanged {
		t.Error("config should be detected as changed")
	}
}

func TestDiff_FilesAddedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "existing.db"), []byte("data"), 0o644)

	provider := &mockProvider{
		config:   json.RawMessage(`{}`),
		webhooks: json.RawMessage(`[]`),
	}

	snap := &Snapshot{
		Config:   json.RawMessage(`{}`),
		Webhooks: json.RawMessage(`[]`),
		DataFiles: []DataFile{
			{Path: "new.db", Size: 100},     // added
			{Path: "existing.db", Size: 99}, // modified (different size)
		},
	}

	diff, err := Diff(provider, snap, dir)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(diff.FilesAdded) != 1 || diff.FilesAdded[0] != "new.db" {
		t.Errorf("files added: %v", diff.FilesAdded)
	}
	if len(diff.FilesModified) != 1 || diff.FilesModified[0] != "existing.db" {
		t.Errorf("files modified: %v", diff.FilesModified)
	}
}

func TestJsonEqual(t *testing.T) {
	a := json.RawMessage(`{"a":1,"b":2}`)
	b := json.RawMessage(`{"b":2,"a":1}`) // same content, different key order
	if !jsonEqual(a, b) {
		t.Error("should be equal regardless of key order")
	}

	c := json.RawMessage(`{"a":1,"b":3}`)
	if jsonEqual(a, c) {
		t.Error("should not be equal")
	}
}
