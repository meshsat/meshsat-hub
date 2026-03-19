package tor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewService_FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostname")
	if err := os.WriteFile(path, []byte("abcdef1234567890.onion\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewService(path)
	info := svc.Info()
	if !info.Available {
		t.Error("expected available=true")
	}
	if info.HTTPAddress != "abcdef1234567890.onion" {
		t.Errorf("http = %q, want abcdef1234567890.onion", info.HTTPAddress)
	}
	if info.MQTTAddress != "abcdef1234567890.onion" {
		t.Errorf("mqtt = %q", info.MQTTAddress)
	}
}

func TestNewService_FileNotFound(t *testing.T) {
	svc := NewService("/nonexistent/path/hostname")
	info := svc.Info()
	if info.Available {
		t.Error("expected available=false when file missing")
	}
}

func TestNewService_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostname")
	_ = os.WriteFile(path, []byte(""), 0o600)

	svc := NewService(path)
	if svc.Info().Available {
		t.Error("expected available=false for empty file")
	}
}

func TestAPIHandler_GetOnion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostname")
	_ = os.WriteFile(path, []byte("test123.onion\n"), 0o600)

	svc := NewService(path)
	h := NewAPIHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/tor/onion", nil)
	w := httptest.NewRecorder()
	h.GetOnion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var info OnionInfo
	_ = json.NewDecoder(w.Body).Decode(&info)
	if info.HTTPAddress != "test123.onion" {
		t.Errorf("http = %q, want test123.onion", info.HTTPAddress)
	}
	if !info.Available {
		t.Error("expected available=true")
	}
}

func TestAPIHandler_GetOnion_NotAvailable(t *testing.T) {
	svc := NewService("/nonexistent")
	h := NewAPIHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/tor/onion", nil)
	w := httptest.NewRecorder()
	h.GetOnion(w, req)

	var info OnionInfo
	_ = json.NewDecoder(w.Body).Decode(&info)
	if info.Available {
		t.Error("expected available=false")
	}
}
