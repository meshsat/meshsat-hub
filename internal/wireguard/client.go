// Package wireguard provides a Go client for wg-easy REST API.
// Enables auto-provisioning of WireGuard peers when devices register with the Hub.
package wireguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client communicates with the wg-easy REST API.
type Client struct {
	baseURL    string
	password   string
	httpClient *http.Client
	sessionID  string
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
		baseURL:    strings.TrimRight(baseURL, "/"),
		password:   password,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Login authenticates with wg-easy and stores the session cookie.
func (c *Client) Login(ctx context.Context) error {
	body := fmt.Sprintf(`{"password":"%s"}`, c.password)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/session", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
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
			c.sessionID = cookie.Value
			slog.Debug("wg-easy: logged in", "session", c.sessionID[:8]+"...")
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
	body := fmt.Sprintf(`{"name":"%s"}`, name)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/wireguard/client", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addSession(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wg-easy create peer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
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
	req, err := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/wireguard/client/%s", c.baseURL, peerID), nil)
	if err != nil {
		return err
	}
	c.addSession(req)

	resp, err := c.httpClient.Do(req)
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
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.addSession(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wg-easy GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wg-easy GET %s: HTTP %d: %s", path, resp.StatusCode, string(data))
	}
	return data, nil
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
	if c.sessionID != "" {
		req.AddCookie(&http.Cookie{Name: "connect.sid", Value: c.sessionID})
	}
}
