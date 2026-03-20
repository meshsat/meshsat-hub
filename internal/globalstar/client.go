// Package globalstar provides a REST API client for the Globalstar SimpleTRAC
// satellite constellation. Messages are sent via the Globalstar Gateway API
// and received via webhook.
//
// Globalstar uses a simplex/duplex model with 128-byte bidirectional payloads.
// Larger messages require chaining (2+ segments).
package globalstar

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
	// MaxPayloadBytes is the Globalstar bidirectional message size limit.
	MaxPayloadBytes = 128

	// DefaultAPIURL is the Globalstar Gateway API base URL.
	DefaultAPIURL = "https://api.globalstar.com/v1"

	// DefaultCostPerMessage is the approximate cost per Globalstar message (USD).
	DefaultCostPerMessage = 0.02
)

// Client communicates with the Globalstar Gateway REST API for message delivery
// and device management.
type Client struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// SendRequest is the payload sent to queue a downlink (MT) message.
type SendRequest struct {
	DeviceID string `json:"deviceId"`
	Data     string `json:"data"` // base64-encoded bytes
}

// SendResponse is returned by the Globalstar message API.
type SendResponse struct {
	ID     string `json:"messageId"`
	Status string `json:"status"` // "queued", "sent", "delivered", "failed"
	Error  string `json:"error,omitempty"`
}

// MessageStatus is the delivery status of a previously sent message.
type MessageStatus struct {
	ID        string `json:"messageId"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DeviceStatus contains device health information.
type DeviceStatus struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name,omitempty"`
	LastSeen string `json:"lastActivityAt,omitempty"`
	Online   bool   `json:"online"`
}

// NewClient creates a new Globalstar API client.
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
// Payload must be <= 128 bytes.
func (c *Client) SendMessage(ctx context.Context, deviceID string, payload []byte) (*SendResponse, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("globalstar: payload %d bytes exceeds limit %d", len(payload), MaxPayloadBytes)
	}

	reqBody := SendRequest{
		DeviceID: deviceID,
		Data:     base64.StdEncoding.EncodeToString(payload),
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("globalstar: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/messages", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("globalstar: create request: %w", err)
	}
	c.setHeaders(req)

	slog.Debug("globalstar: sending message", "device", deviceID, "bytes", len(payload), "url", url)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("globalstar: send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("globalstar: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("globalstar: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var sendResp SendResponse
	if err := json.Unmarshal(body, &sendResp); err != nil {
		return nil, fmt.Errorf("globalstar: parse response: %w", err)
	}

	slog.Info("globalstar: message queued", "device", deviceID, "id", sendResp.ID, "status", sendResp.Status)
	return &sendResp, nil
}

// CheckMessageStatus queries the delivery status of a previously sent message.
func (c *Client) CheckMessageStatus(ctx context.Context, messageID string) (*MessageStatus, error) {
	url := fmt.Sprintf("%s/messages/%s", c.apiURL, messageID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("globalstar: create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("globalstar: check status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("globalstar: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("globalstar: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var status MessageStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("globalstar: parse status: %w", err)
	}

	return &status, nil
}

// GetDeviceStatus queries the status of a Globalstar device.
func (c *Client) GetDeviceStatus(ctx context.Context, deviceID string) (*DeviceStatus, error) {
	url := fmt.Sprintf("%s/devices/%s", c.apiURL, deviceID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("globalstar: create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("globalstar: get device status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("globalstar: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("globalstar: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var status DeviceStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("globalstar: parse device status: %w", err)
	}

	return &status, nil
}

// IsReachable performs a lightweight health check against the Globalstar API.
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}
