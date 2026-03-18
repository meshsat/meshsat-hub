package hawkbit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(handler http.HandlerFunc) (*httptest.Server, *Client) {
	server := httptest.NewServer(handler)
	client := NewClient(server.URL, "admin", "admin")
	return server, client
}

func TestCreateTarget(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/rest/v1/targets" {
			t.Errorf("expected /rest/v1/targets, got %s", r.URL.Path)
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "admin" {
			t.Error("missing or wrong basic auth")
		}

		var targets []Target
		if err := json.NewDecoder(r.Body).Decode(&targets); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(targets) != 1 || targets[0].ControllerId != "device-001" {
			t.Errorf("unexpected target: %+v", targets)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Target{{ControllerId: "device-001", Name: "Field Unit 1"}})
	})
	defer server.Close()

	target, err := client.CreateTarget(context.Background(), Target{
		ControllerId: "device-001",
		Name:         "Field Unit 1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.ControllerId != "device-001" {
		t.Errorf("expected device-001, got %s", target.ControllerId)
	}
}

func TestGetTarget(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/targets/device-001" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Target{
			ControllerId: "device-001",
			Name:         "Field Unit 1",
			UpdateStatus: "in_sync",
		})
	})
	defer server.Close()

	target, err := client.GetTarget(context.Background(), "device-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.UpdateStatus != "in_sync" {
		t.Errorf("expected in_sync, got %s", target.UpdateStatus)
	}
}

func TestListTargets(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := listResponse{
			Content: json.RawMessage(`[{"controllerId":"d1"},{"controllerId":"d2"}]`),
			Total:   2,
			Size:    2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer server.Close()

	targets, err := client.ListTargets(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("expected 2 targets, got %d", len(targets))
	}
}

func TestDeleteTarget(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/rest/v1/targets/device-001" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if err := client.DeleteTarget(context.Background(), "device-001"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateSoftwareModule(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]SoftwareModule{{ID: 1, Name: "meshsat-fw", Version: "0.3.0"}})
	})
	defer server.Close()

	module, err := client.CreateSoftwareModule(context.Background(), SoftwareModule{
		Name:    "meshsat-fw",
		Version: "0.3.0",
		Type:    "firmware",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if module.ID != 1 {
		t.Errorf("expected ID 1, got %d", module.ID)
	}
}

func TestCreateDistributionSet(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]DistributionSet{{ID: 10, Name: "meshsat-0.3.0", Version: "0.3.0"}})
	})
	defer server.Close()

	ds, err := client.CreateDistributionSet(context.Background(), DistributionSet{
		Name:    "meshsat-0.3.0",
		Version: "0.3.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ds.ID != 10 {
		t.Errorf("expected ID 10, got %d", ds.ID)
	}
}

func TestCreateRollout(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/rest/v1/rollouts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Rollout{ID: 5, Name: "rollout-1", Status: "creating"})
	})
	defer server.Close()

	rollout, err := client.CreateRollout(context.Background(), Rollout{
		Name:            "rollout-1",
		DistributionSet: 10,
		TargetFilter:    "name==meshsat-*",
		GroupCount:      2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rollout.Status != "creating" {
		t.Errorf("expected creating, got %s", rollout.Status)
	}
}

func TestGetRollout(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Rollout{ID: 5, Name: "rollout-1", Status: "running", TotalTargets: 10})
	})
	defer server.Close()

	rollout, err := client.GetRollout(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rollout.TotalTargets != 10 {
		t.Errorf("expected 10 targets, got %d", rollout.TotalTargets)
	}
}

func TestStartRollout(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/rest/v1/rollouts/5/start" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if err := client.StartRollout(context.Background(), 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPauseRollout(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/rollouts/5/pause" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if err := client.PauseRollout(context.Background(), 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetTargetActions(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := listResponse{
			Content: json.RawMessage(`[{"id":1,"type":"update","status":"running"}]`),
			Total:   1,
			Size:    1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer server.Close()

	actions, err := client.GetTargetActions(context.Background(), "device-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Status != "running" {
		t.Errorf("expected running, got %s", actions[0].Status)
	}
}

func TestCancelAction(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if err := client.CancelAction(context.Background(), "device-001", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsReachable(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if !client.IsReachable(context.Background()) {
		t.Error("expected reachable")
	}
}

func TestIsReachable_Down(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()

	if client.IsReachable(context.Background()) {
		t.Error("expected unreachable")
	}
}

func TestAPIError(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	defer server.Close()

	_, err := client.GetTarget(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for 404")
	}
}
