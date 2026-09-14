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

	p := NewProvisionerPool(NewClientPool(c, nil))
	addr, peer, err := p.OnDeviceCreated(context.Background(), "default", "300234063904190")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.8.0.5/32" {
		t.Errorf("address = %s, want 10.8.0.5/32", addr)
	}
	if peer == nil || peer.ID != "peer-123" {
		t.Errorf("peer ID = %v, want peer-123", peer)
	}

	mu.Lock()
	defer mu.Unlock()
	if createdName != "meshsat-300234063904190" {
		t.Errorf("peer name = %s, want meshsat-300234063904190", createdName)
	}

	if p.GetPeerID("default", "300234063904190") != "peer-123" {
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

	p := NewProvisionerPool(NewClientPool(c, nil))
	// Manually set up a tracked peer.
	p.peers[peerKey("default", "dev1")] = "peer-456"

	p.OnDeviceDeleted(context.Background(), "default", "dev1")

	if deletedID != "peer-456" {
		t.Errorf("deleted peer ID = %s, want peer-456", deletedID)
	}
	if p.GetPeerID("default", "dev1") != "" {
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

	p := NewProvisionerPool(NewClientPool(c, nil))
	p.Hydrate(context.Background(), "default")

	if p.GetPeerID("default", "dev001") != "p1" {
		t.Errorf("dev001 peer = %s, want p1", p.GetPeerID("default", "dev001"))
	}
	if p.GetPeerID("default", "dev002") != "p2" {
		t.Errorf("dev002 peer = %s, want p2", p.GetPeerID("default", "dev002"))
	}
	if p.GetPeerID("default", "manual-peer") != "" {
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

	p := NewProvisionerPool(NewClientPool(c, nil))
	p.peers[peerKey("default", "dev1")] = "peer-789"

	cfg, err := p.GetDeviceConfig(context.Background(), "default", "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "10.8.0.5") {
		t.Errorf("config should contain VPN address, got: %s", cfg)
	}
}

func TestProvisioner_GetDeviceConfig_NoPeer(t *testing.T) {
	p := NewProvisionerPool(NewClientPool(nil, nil))
	_, err := p.GetDeviceConfig(context.Background(), "default", "unknown")
	if err == nil {
		t.Error("expected error for unknown device")
	}
}

// The peer map is keyed by tenant AND device (MESHSAT-1121). A device id is only
// unique within a tenant, so keyed on the device alone one tenant's registration
// overwrote another's peer mapping -- and the next delete then removed the WRONG
// tenant's peer, while GetDeviceConfig handed back the wrong peer's PRIVATE KEY.
func TestPeerMappingsAreKeptApartPerTenant(t *testing.T) {
	p := NewProvisionerPool(NewClientPool(nil, nil))

	p.mu.Lock()
	p.peers[peerKey("t_one", "shared-id")] = "peer-one"
	p.peers[peerKey("t_two", "shared-id")] = "peer-two"
	p.mu.Unlock()

	if got := p.GetPeerID("t_one", "shared-id"); got != "peer-one" {
		t.Errorf("tenant one resolved to %q, want peer-one", got)
	}
	if got := p.GetPeerID("t_two", "shared-id"); got != "peer-two" {
		t.Errorf("tenant two resolved to %q, want peer-two: one tenant's peer "+
			"mapping overwrote the other's for the same device id, so a config "+
			"download would hand over the wrong peer's private key", got)
	}
	if got := p.GetPeerID("t_three", "shared-id"); got != "" {
		t.Errorf("a tenant with no peer for this device resolved to %q", got)
	}
}

// A tenant with no wg-easy configured must not have its device registration
// fail: WireGuard is optional, and a device is not a VPN peer.
func TestRegisteringADeviceSucceedsForATenantWithNoVPN(t *testing.T) {
	p := NewProvisionerPool(NewClientPool(nil, nil))
	addr, peer, err := p.OnDeviceCreated(context.Background(), "t_novpn", "300234063904190")
	if err != nil {
		t.Fatalf("registering a device failed for a tenant with no VPN: %v", err)
	}
	if peer != nil || addr != "" {
		t.Errorf("got a peer %v / %q for a tenant with no VPN", peer, addr)
	}
}
