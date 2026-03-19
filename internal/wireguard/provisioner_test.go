package wireguard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestProvisioner_OnDeviceCreated(t *testing.T) {
	var mu sync.Mutex
	var createdName string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/session" && r.Method == "POST":
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "testsession12345678"})
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/api/wireguard/client" && r.Method == "POST":
			var req struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			createdName = req.Name
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Peer{
				ID:      "peer-123",
				Name:    req.Name,
				Address: "10.8.0.5/32",
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testpass")
	_ = c.Login(context.Background())

	p := NewProvisioner(c)
	addr, err := p.OnDeviceCreated(context.Background(), "300234063904190")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.8.0.5/32" {
		t.Errorf("address = %s, want 10.8.0.5/32", addr)
	}

	mu.Lock()
	defer mu.Unlock()
	if createdName != "meshsat-300234063904190" {
		t.Errorf("peer name = %s, want meshsat-300234063904190", createdName)
	}

	if p.GetPeerID("300234063904190") != "peer-123" {
		t.Error("peer ID not tracked")
	}
}

func TestProvisioner_OnDeviceDeleted(t *testing.T) {
	var deletedID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/session":
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "testsession12345678"})
			w.WriteHeader(http.StatusOK)

		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/wireguard/client/"):
			deletedID = strings.TrimPrefix(r.URL.Path, "/api/wireguard/client/")
			w.WriteHeader(http.StatusNoContent)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testpass")
	_ = c.Login(context.Background())

	p := NewProvisioner(c)
	// Manually set up a tracked peer.
	p.peers["dev1"] = "peer-456"

	p.OnDeviceDeleted(context.Background(), "dev1")

	if deletedID != "peer-456" {
		t.Errorf("deleted peer ID = %s, want peer-456", deletedID)
	}
	if p.GetPeerID("dev1") != "" {
		t.Error("peer should be removed from tracking")
	}
}

func TestProvisioner_Hydrate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/session":
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "testsession12345678"})
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/api/wireguard/client" && r.Method == "GET":
			peers := []Peer{
				{ID: "p1", Name: "meshsat-dev001", Address: "10.8.0.2/32"},
				{ID: "p2", Name: "meshsat-dev002", Address: "10.8.0.3/32"},
				{ID: "p3", Name: "manual-peer", Address: "10.8.0.4/32"}, // not meshsat-
			}
			_ = json.NewEncoder(w).Encode(peers)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testpass")
	_ = c.Login(context.Background())

	p := NewProvisioner(c)
	p.Hydrate(context.Background())

	if p.GetPeerID("dev001") != "p1" {
		t.Errorf("dev001 peer = %s, want p1", p.GetPeerID("dev001"))
	}
	if p.GetPeerID("dev002") != "p2" {
		t.Errorf("dev002 peer = %s, want p2", p.GetPeerID("dev002"))
	}
	if p.GetPeerID("manual-peer") != "" {
		t.Error("manual-peer should not be tracked")
	}
}

func TestProvisioner_GetDeviceConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/session":
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "testsession12345678"})
		case strings.HasSuffix(r.URL.Path, "/configuration"):
			_, _ = w.Write([]byte("[Interface]\nAddress = 10.8.0.5/32\nPrivateKey = xxx\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testpass")
	_ = c.Login(context.Background())

	p := NewProvisioner(c)
	p.peers["dev1"] = "peer-789"

	cfg, err := p.GetDeviceConfig(context.Background(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "10.8.0.5") {
		t.Errorf("config should contain VPN address, got: %s", cfg)
	}
}

func TestProvisioner_GetDeviceConfig_NoPeer(t *testing.T) {
	p := NewProvisioner(nil)
	_, err := p.GetDeviceConfig(context.Background(), "unknown")
	if err == nil {
		t.Error("expected error for unknown device")
	}
}
