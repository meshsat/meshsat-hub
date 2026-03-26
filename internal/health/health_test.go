package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	c := New(3 * time.Second)
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
	c := New(3 * time.Second)
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
	if resp.Checks["mqtt"].Status != "unhealthy" {
		t.Errorf("expected mqtt unhealthy, got %s", resp.Checks["mqtt"].Status)
	}
}

func TestReadyzHandler_ProbeHealthy(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("mqtt", true)
	c.AddProbe("postgres", func(ctx context.Context) error {
		return nil
	})
	c.AddProbe("redis", func(ctx context.Context) error {
		return nil
	})

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
	if resp.Checks["postgres"].Status != "ok" {
		t.Errorf("expected postgres ok, got %s", resp.Checks["postgres"].Status)
	}
	if resp.Checks["redis"].Status != "ok" {
		t.Errorf("expected redis ok, got %s", resp.Checks["redis"].Status)
	}
}

func TestReadyzHandler_ProbeUnhealthy(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("mqtt", true)
	c.AddProbe("postgres", func(ctx context.Context) error {
		return fmt.Errorf("connection refused")
	})
	c.AddProbe("redis", func(ctx context.Context) error {
		return nil
	})

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
	if resp.Checks["postgres"].Status != "unhealthy" {
		t.Errorf("expected postgres unhealthy, got %s", resp.Checks["postgres"].Status)
	}
	if resp.Checks["redis"].Status != "ok" {
		t.Errorf("expected redis ok, got %s", resp.Checks["redis"].Status)
	}
}

func TestReadyzHandler_MixedStaticAndProbe(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("mqtt", true)
	c.AddProbe("postgres", func(ctx context.Context) error {
		return nil
	})

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
	if len(resp.Checks) != 2 {
		t.Errorf("expected 2 checks, got %d", len(resp.Checks))
	}
}

func TestStartupzHandler_NotReady(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("mqtt", false)

	req := httptest.NewRequest("GET", "/startupz", nil)
	w := httptest.NewRecorder()
	c.StartupzHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestStartupzHandler_BecomesReady(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("mqtt", true)

	req := httptest.NewRequest("GET", "/startupz", nil)
	w := httptest.NewRecorder()
	c.StartupzHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Once ready, stays ready even if probe fails.
	c.Set("mqtt", false)
	req2 := httptest.NewRequest("GET", "/startupz", nil)
	w2 := httptest.NewRecorder()
	c.StartupzHandler(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 (startup already complete), got %d", w2.Code)
	}
}

func TestDetailedProbe(t *testing.T) {
	c := New(3 * time.Second)
	c.AddDetailedProbe("galera", func(ctx context.Context) (map[string]any, error) {
		return map[string]any{
			"cluster_size": 3,
			"node_state":   "Synced",
		}, nil
	})

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
	cr := resp.Checks["galera"]
	if cr == nil {
		t.Fatal("expected galera check in response")
	}
	if cr.Status != "ok" {
		t.Errorf("expected ok, got %s", cr.Status)
	}
	if cr.Detail["cluster_size"] != float64(3) {
		t.Errorf("expected cluster_size 3, got %v", cr.Detail["cluster_size"])
	}
}
