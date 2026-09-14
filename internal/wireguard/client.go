// Package wireguard provides a Go client for wg-easy REST API.
// Enables auto-provisioning of WireGuard peers when devices register with the Hub.
package wireguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/fsutil"
	"github.com/meshsat/meshsat-hub/internal/netguard"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client communicates with the wg-easy REST API.
type Client struct {
	baseURL    string
	password   string
	httpClient *http.Client

	// mu guards sessionID. It is written by Login and read by addSession on
	// every request, and the provisioner is driven from HTTP handlers, so the
	// two race. That was true before the lazy login below and is why `go test
	// -race` has to be run by hand -- CI cannot (CGO_ENABLED=0).
	mu        sync.Mutex
	sessionID string
}

// Peer represents a WireGuard peer from wg-easy.
type Peer struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	PublicKey         string    `json:"publicKey"`
	PreSharedKey      string    `json:"preSharedKey,omitempty"`
	Address           string    `json:"address"`
	LatestHandshakeAt time.Time `json:"latestHandshakeAt,omitempty"`
	TransferRx        int64     `json:"transferRx"`
	TransferTx        int64     `json:"transferTx"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// PeerConfig is the client config for a peer (shown as QR code / downloadable).
type PeerConfig struct {
	Config string `json:"config"` // WireGuard INI-style config
}

// NewClient creates a new wg-easy API client.
func NewClient(baseURL, password string) *Client {
	return &Client{
		baseURL:  validatedBaseURL(baseURL),
		password: password,
		// so the address is checked immediately before connect, after resolution.
		// integrations.Set checks it on save too, but only this survives DNS
		// rebinding -- a name that resolved publicly then can resolve to
		// 127.0.0.1 now, and the Hub is in a cluster full of reachable services.
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Login authenticates with wg-easy and stores the session cookie.
func (c *Client) Login(ctx context.Context) error {
	data, err := json.Marshal(struct {
		Password string `json:"password"`
	}{Password: c.password})
	if err != nil {
		return fmt.Errorf("wg-easy login: marshal: %w", err)
	}
	// #nosec G704 -- same two guards as every other request in this file, and
	// the taint reaches Login only because ensureSession now calls it from the
	// request path: integrations.Set validates the URL against netguard before
	// storing it, and a tenant-built client dials through
	// netguard.SafeHTTPClient, which refuses a non-public address after
	// resolution. See the note on CreatePeer.
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/session", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req) // #nosec G704 -- see the note above this request
	if err != nil {
		return fmt.Errorf("wg-easy login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("wg-easy login: HTTP %d", resp.StatusCode)
	}

	// Extract session cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "connect.sid" {
			c.mu.Lock()
			c.sessionID = cookie.Value
			c.mu.Unlock()
			slog.Debug("wg-easy: logged in", "session", cookie.Value[:8]+"...")
			return nil
		}
	}

	return fmt.Errorf("wg-easy: no session cookie in response")
}

// ListPeers returns all WireGuard peers.
func (c *Client) ListPeers(ctx context.Context) ([]Peer, error) {
	data, err := c.get(ctx, "/api/wireguard/client")
	if err != nil {
		return nil, err
	}
	var peers []Peer
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, fmt.Errorf("wg-easy: parse peers: %w", err)
	}
	return peers, nil
}

// CreatePeer creates a new WireGuard peer with the given name.
func (c *Client) CreatePeer(ctx context.Context, name string) (*Peer, error) {
	data, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: name})
	if err != nil {
		return nil, fmt.Errorf("wg-easy create peer: marshal: %w", err)
	}
	// #nosec G704 -- SSRF is real here and is guarded TWICE, neither of which
	// gosec's taint analysis can follow across a package boundary or into a
	// dialer. c.baseURL comes from a tenant's integrations account since
	// MESHSAT-1121 (it was an operator-set env var before, which is why this
	// finding appeared with that change and is NOT a false positive):
	//   1. integrations.Set runs netguard.ValidatePublicURL before storing it,
	//      so an internal name or address is refused at the form.
	//   2. c.httpClient is netguard.SafeHTTPClient, whose dialer refuses to
	//      CONNECT to a non-public address after resolution -- which is the half
	//      that survives DNS rebinding, and the one that actually holds.
	// Removing either guard reopens it; internal/netguard's tests cover both.
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/wireguard/client", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addSession(req)

	resp, err := c.httpClient.Do(req) // #nosec G704 -- see the note above this request

	if err != nil {
		return nil, fmt.Errorf("wg-easy create peer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wg-easy create: HTTP %d: %s", resp.StatusCode, string(data))
	}

	var peer Peer
	if err := json.Unmarshal(data, &peer); err != nil {
		return nil, fmt.Errorf("wg-easy: parse peer: %w", err)
	}
	slog.Info("wg-easy: peer created", "name", name, "id", peer.ID, "address", peer.Address)
	return &peer, nil
}

// GetPeerConfig returns the WireGuard client configuration for a peer.
func (c *Client) GetPeerConfig(ctx context.Context, peerID string) (string, error) {
	data, err := c.get(ctx, fmt.Sprintf("/api/wireguard/client/%s/configuration", peerID))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DeletePeer removes a WireGuard peer.
func (c *Client) DeletePeer(ctx context.Context, peerID string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/wireguard/client/%s", c.baseURL, url.PathEscape(peerID)), nil) // #nosec G704 -- operator-configured wg-easy URL validated in NewClient
	if err != nil {
		return err
	}
	c.addSession(req)

	resp, err := c.httpClient.Do(req) // #nosec G704 -- see NewClient
	if err != nil {
		return fmt.Errorf("wg-easy delete: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("wg-easy delete: HTTP %d", resp.StatusCode)
	}
	return nil
}

// EnablePeer enables a disabled peer.
func (c *Client) EnablePeer(ctx context.Context, peerID string) error {
	return c.post(ctx, fmt.Sprintf("/api/wireguard/client/%s/enable", peerID))
}

// DisablePeer disables a peer without removing it.
func (c *Client) DisablePeer(ctx context.Context, peerID string) error {
	return c.post(ctx, fmt.Sprintf("/api/wireguard/client/%s/disable", peerID))
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	data, status, err := c.rawGet(ctx, path)
	if status == http.StatusUnauthorized {
		// The session expired or wg-easy restarted under us. Log in and retry
		// ONCE -- a loop here would hammer a wg-easy that is simply refusing us.
		c.clearSession()
		if err := c.ensureSession(ctx); err != nil {
			return nil, err
		}
		data, _, err = c.rawGet(ctx, path)
	}
	return data, err
}

func (c *Client) rawGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil) // #nosec G704 -- see NewClient
	if err != nil {
		return nil, 0, err
	}
	c.addSession(req)

	resp, err := c.httpClient.Do(req) // #nosec G704 -- see NewClient
	if err != nil {
		return nil, 0, fmt.Errorf("wg-easy GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("wg-easy GET %s: HTTP %d: %s", path, resp.StatusCode, string(data))
	}
	return data, resp.StatusCode, nil
}

func (c *Client) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.addSession(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("wg-easy POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("wg-easy POST %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

func (c *Client) addSession(req *http.Request) {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "connect.sid", Value: sid})
	}
}

// validatedBaseURL normalises the operator-configured wg-easy URL; an invalid
// value is logged at startup and kept so the first request fails visibly.
func validatedBaseURL(raw string) string {
	v, err := fsutil.ValidateBaseURL(raw)
	if err != nil {
		slog.Error("wireguard: invalid wg-easy URL, requests will fail", "error", err)
		return strings.TrimRight(raw, "/")
	}
	return v
}

// maybeGuard makes this client refuse to CONNECT to an address the Hub keeps
// on its own side of the wire. Called by the POOL, on a client built from a
// TENANT's URL -- never on the platform's own, whose http://wg-easy:51821 is exactly what
// the guard refuses and is correct for the operator to use (MESHSAT-1121).
func (c *Client) maybeGuard(skip bool, timeout time.Duration) *Client {
	if skip {
		return c
	}
	c.httpClient = netguard.SafeHTTPClient(timeout)
	return c
}

// ensureSession logs in when there is no session yet.
//
// wg-easy's session is a cookie that dies whenever wg-easy restarts, and the
// Hub used to log in exactly once at startup and never again. Two consequences,
// both seen in production on the day this was deployed (MESHSAT-1121):
//
//   - A wg-easy that was not up yet when the Hub booted left the Hub with no
//     session at all, and nothing retried.
//   - A wg-easy pod restart -- a node drain, an image bump -- silently broke
//     every call until somebody restarted the HUB.
//
// Logging in on demand removes the startup-order dependency entirely: it no
// longer matters which of the two starts first, or how often the other restarts.
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	have := c.sessionID != ""
	c.mu.Unlock()
	if have {
		return nil
	}
	return c.Login(ctx)
}

// clearSession drops a session wg-easy has stopped honouring, so the next call
// logs in again rather than repeating a request that will keep failing.
func (c *Client) clearSession() {
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()
}
