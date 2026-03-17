package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLivezHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	LivezHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status ok, got %s", resp.Status)
	}
}

func TestReadyzHandler_AllHealthy(t *testing.T) {
	c := New()
	c.Set("mqtt", true)
	c.Set("cloudloop", true)

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	c.ReadyzHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected ok, got %s", resp.Status)
	}
}

func TestReadyzHandler_Unhealthy(t *testing.T) {
	c := New()
	c.Set("mqtt", false)
	c.Set("cloudloop", true)

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	c.ReadyzHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "unhealthy" {
		t.Errorf("expected unhealthy, got %s", resp.Status)
	}
	if resp.Checks["mqtt"] != "unhealthy" {
		t.Errorf("expected mqtt unhealthy, got %s", resp.Checks["mqtt"])
	}
}
