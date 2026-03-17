package cloudloop

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client communicates with the Cloudloop/Ground Control REST API for MT message delivery.
type Client struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// MTRequest is the payload sent to the Cloudloop MT API.
type MTRequest struct {
	IMEI string `json:"imei"`
	Data string `json:"data"` // hex-encoded bytes
}

// MTResponse is returned by the Cloudloop MT API.
type MTResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// NewClient creates a new Cloudloop API client.
func NewClient(apiURL, apiKey string) *Client {
	return &Client{
		apiURL: apiURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SendMT sends a Mobile Terminated message to a device via the Cloudloop API.
func (c *Client) SendMT(ctx context.Context, imei string, payload []byte) (*MTResponse, error) {
	reqBody := MTRequest{
		IMEI: imei,
		Data: hex.EncodeToString(payload),
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/sbd/mt", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	slog.Debug("cloudloop: sending MT", "imei", imei, "bytes", len(payload), "url", url)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: send MT: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloudloop: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var mtResp MTResponse
	if err := json.Unmarshal(body, &mtResp); err != nil {
		return nil, fmt.Errorf("cloudloop: parse response: %w", err)
	}

	slog.Info("cloudloop: MT queued", "imei", imei, "id", mtResp.ID, "status", mtResp.Status)
	return &mtResp, nil
}

// CheckMTStatus checks the delivery status of a previously sent MT message.
func (c *Client) CheckMTStatus(ctx context.Context, mtID string) (*MTResponse, error) {
	url := fmt.Sprintf("%s/sbd/mt/%s", c.apiURL, mtID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: check MT status: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloudloop: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var mtResp MTResponse
	if err := json.Unmarshal(body, &mtResp); err != nil {
		return nil, fmt.Errorf("cloudloop: parse response: %w", err)
	}

	return &mtResp, nil
}

// IsReachable performs a lightweight check to see if the API is reachable.
func (c *Client) IsReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", c.apiURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}
