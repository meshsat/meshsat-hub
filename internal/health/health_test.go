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

func TestInfoProbeNeverAffectsReadiness(t *testing.T) {
	c := New(3 * time.Second)
	c.AddProbe("db", func(ctx context.Context) error { return nil })
	c.AddInfoProbe("mqtt", func(ctx context.Context) error { return fmt.Errorf("mqtt not connected") })

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	c.ReadyzHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with a failing info probe, got %d", w.Code)
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected ok, got %s", resp.Status)
	}
	if _, present := resp.Checks["mqtt"]; present {
		t.Errorf("info probe must not appear under checks")
	}
	if resp.Info != nil {
		t.Errorf("info must be omitted without ?verbose=1, got %v", resp.Info)
	}

	// Verbose lists it with the error, still 200 -- but only for a caller
	// presenting the operator token. The detail is each dependency's raw error
	// string, which names internal hosts and ports, so an anonymous caller is
	// downgraded to the plain response rather than shown it.
	req = httptest.NewRequest("GET", "/readyz?verbose=1", nil)
	w = httptest.NewRecorder()
	c.ReadyzHandler(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Info != nil {
		t.Errorf("verbose detail served to an anonymous caller: %v", resp.Info)
	}

	c.SetDiagnosticsToken("op-token")
	req = httptest.NewRequest("GET", "/readyz?verbose=1", nil)
	req.Header.Set("Authorization", "Bearer op-token")
	w = httptest.NewRecorder()
	c.ReadyzHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verbose: expected 200, got %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	cr := resp.Info["mqtt"]
	if cr == nil || cr.Status != "unhealthy" || cr.Detail["error"] != "mqtt not connected" {
		t.Errorf("verbose info entry wrong: %+v", cr)
	}
	if resp.Checks["db"] == nil || resp.Checks["db"].Status != "ok" {
		t.Errorf("critical db check missing or wrong: %+v", resp.Checks["db"])
	}
}

func TestDrainingReturns503(t *testing.T) {
	c := New(3 * time.Second)
	c.AddProbe("db", func(ctx context.Context) error { return nil })
	c.SetDraining()
	if !c.Draining() {
		t.Fatal("Draining() should be true")
	}

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	c.ReadyzHandler(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %d", w.Code)
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "draining" {
		t.Errorf("expected status draining, got %s", resp.Status)
	}
	if resp.Checks["db"] == nil || resp.Checks["db"].Status != "ok" {
		t.Errorf("checks should still be reported while draining: %+v", resp.Checks["db"])
	}
}

func TestMarkStartedSkipsProbes(t *testing.T) {
	c := New(3 * time.Second)
	c.Set("db", false)
	c.MarkStarted()

	req := httptest.NewRequest("GET", "/startupz", nil)
	w := httptest.NewRecorder()
	c.StartupzHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after MarkStarted regardless of probes, got %d", w.Code)
	}
}
