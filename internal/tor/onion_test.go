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

// The address may come from configuration rather than a file, because on
// Kubernetes the Hub cannot mount Tor's key volume -- separate pods, RWO
// node-local storage (MESHSAT-1121).
func TestTheOnionAddressCanComeFromConfiguration(t *testing.T) {
	const addr = "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwxyz23456.onion"

	s := NewServiceFor(addr, "/nonexistent/hostname")
	info := s.Info()
	if !info.Available {
		t.Fatal("a configured .onion address was reported unavailable: /api/tor/onion " +
			"would say the Hub has no onion service when it has one")
	}
	if info.HTTPAddress != addr || info.MQTTAddress != addr {
		t.Errorf("addresses = %q / %q, want %q", info.HTTPAddress, info.MQTTAddress, addr)
	}
}

// With nothing configured it still falls back to the file, which is what a
// compose or single-host deployment uses.
func TestWithNoConfiguredAddressItStillReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hostname"
	if err := os.WriteFile(path, []byte("fromfile234567abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnop.onion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServiceFor("", path)
	if !s.Info().Available {
		t.Fatal("the file fallback stopped working; a compose deployment would lose its onion")
	}
}

// Whitespace is the realistic failure: the hostname file ends with a newline and
// somebody will paste its contents into a ConfigMap.
func TestAConfiguredAddressIsTrimmed(t *testing.T) {
	s := NewServiceFor("  padded234567abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnop.onion\n", "")
	if got := s.Info().HTTPAddress; got != "padded234567abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnop.onion" {
		t.Errorf("address = %q, want it trimmed", got)
	}
}
