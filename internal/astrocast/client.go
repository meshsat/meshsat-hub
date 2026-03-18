// Package astrocast provides a REST API client for the Astrocast Astronode S
// satellite constellation. Messages are sent via the Astrocast Portal API and
// received via webhook or polling.
//
// API reference: https://docs.astrocast.com/docs/api/overview
// Max message size: 160 bytes (uplink and downlink).
package astrocast

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// MaxPayloadBytes is the Astrocast downlink (MT) message size limit.
	MaxPayloadBytes = 160

	// DefaultAPIURL is the Astrocast Portal API base URL.
	DefaultAPIURL = "https://api.astrocast.com/v1"

	// DefaultCostPerMessage is the approximate cost per Astrocast message (USD).
	DefaultCostPerMessage = 0.01
)

// Client communicates with the Astrocast Portal REST API for message delivery
// and device management.
type Client struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// SendRequest is the payload sent to queue a downlink message.
type SendRequest struct {
	DeviceGUID string `json:"deviceGuid"`
	Data       string `json:"data"` // base64-encoded bytes
}

// SendResponse is returned by the Astrocast message API.
type SendResponse struct {
	ID     string `json:"commandId"`
	Status string `json:"status"` // "queued", "sent", "delivered", "failed", "expired"
	Error  string `json:"error,omitempty"`
}

// MessageStatus is the delivery status of a previously sent message.
type MessageStatus struct {
	ID        string `json:"commandId"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DeviceStatus contains device health information.
type DeviceStatus struct {
	DeviceGUID string `json:"deviceGuid"`
	Name       string `json:"name,omitempty"`
	LastSeen   string `json:"lastActivityAt,omitempty"`
	Online     bool   `json:"online"`
}

// NewClient creates a new Astrocast API client.
func NewClient(apiURL, apiKey string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Client{
		apiURL: apiURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SendMessage queues a downlink (MT) message for delivery to a device.
// The deviceID should be the Astrocast device GUID. Payload must be <= 160 bytes.
func (c *Client) SendMessage(ctx context.Context, deviceID string, payload []byte) (*SendResponse, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("astrocast: payload %d bytes exceeds limit %d", len(payload), MaxPayloadBytes)
	}

	reqBody := SendRequest{
		DeviceGUID: deviceID,
		Data:       base64.StdEncoding.EncodeToString(payload),
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("astrocast: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/commands", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("astrocast: create request: %w", err)
	}
	c.setHeaders(req)

	slog.Debug("astrocast: sending message", "device", deviceID, "bytes", len(payload), "url", url)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("astrocast: send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("astrocast: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("astrocast: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var sendResp SendResponse
	if err := json.Unmarshal(body, &sendResp); err != nil {
		return nil, fmt.Errorf("astrocast: parse response: %w", err)
	}

	slog.Info("astrocast: message queued", "device", deviceID, "id", sendResp.ID, "status", sendResp.Status)
	return &sendResp, nil
}

// CheckMessageStatus queries the delivery status of a previously sent message.
func (c *Client) CheckMessageStatus(ctx context.Context, commandID string) (*MessageStatus, error) {
	url := fmt.Sprintf("%s/commands/%s", c.apiURL, commandID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("astrocast: create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("astrocast: check status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("astrocast: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("astrocast: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var status MessageStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("astrocast: parse status: %w", err)
	}

	return &status, nil
}

// GetDeviceStatus queries the status of an Astrocast device.
func (c *Client) GetDeviceStatus(ctx context.Context, deviceGUID string) (*DeviceStatus, error) {
	url := fmt.Sprintf("%s/devices/%s", c.apiURL, deviceGUID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("astrocast: create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("astrocast: get device status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("astrocast: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("astrocast: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var status DeviceStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("astrocast: parse device status: %w", err)
	}

	return &status, nil
}

// IsReachable performs a lightweight health check against the Astrocast API.
func (c *Client) IsReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/devices", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
}
