package cloudloop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// IMT topic constants for DoSendImtMessage.
const (
	IMTTopicPurple     = "IMT_TOPIC_PURPLE"
	IMTTopicPink       = "IMT_TOPIC_PINK"
	IMTTopicRed        = "IMT_TOPIC_RED"
	IMTTopicOrange     = "IMT_TOPIC_ORANGE"
	IMTTopicYellow     = "IMT_TOPIC_YELLOW"
	IMTTopicRaw        = "IMT_TOPIC_RAW"
	IMTTopicRockRemote = "IMT_TOPIC_ROCKREMOTE_CDM"
)

// Ring style constants for DoSendImtMessage.
const (
	RingStyleNormal   = "RING_STYLE_NORMAL"
	RingStyleUrgent   = "RING_STYLE_URGENT"
	RingStyleExtended = "RING_STYLE_EXTENDED"
)

// Client communicates with the Cloudloop/Ground Control Data API for MT message delivery.
// Supports both legacy custom endpoints and the official Cloudloop Data API.
type Client struct {
	apiURL     string
	apiKey     string // used as token query parameter for official API
	httpClient *http.Client
}

// MTResponse is returned by the Cloudloop API.
type MTResponse struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	MTMessageStatus int    `json:"mtMessageStatus,omitempty"`
}

// NewClient creates a new Cloudloop API client.
// apiURL is the base URL (default: https://api.cloudloop.com).
// apiKey is the authentication token (UUID format).
func NewClient(apiURL, apiKey string) *Client {
	return &Client{
		apiURL: apiURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SendSBD sends a Mobile Terminated SBD message via the official Cloudloop Data API.
// thingID is the Cloudloop device identifier (32-char alphanumeric, NOT the IMEI).
// payload is hex-encoded by this method (max 270 bytes raw).
func (c *Client) SendSBD(ctx context.Context, thingID string, payload []byte) (*MTResponse, error) {
	params := url.Values{
		"token":   {c.apiKey},
		"thing":   {thingID},
		"message": {hex.EncodeToString(payload)},
	}

	apiURL := fmt.Sprintf("%s/Data/DoSendSbdMessage?%s", c.apiURL, params.Encode())
	slog.Debug("cloudloop: sending SBD MT", "thing", thingID, "bytes", len(payload))

	return c.doPost(ctx, apiURL, "SBD MT")
}

// SendIMT sends a Mobile Terminated IMT message via the official Cloudloop Data API.
// thingID is the Cloudloop device identifier. payload is base64-encoded by this method (max 100KB raw).
// topic defaults to IMTTopicRaw if empty. ringStyle defaults to RingStyleNormal if empty.
func (c *Client) SendIMT(ctx context.Context, thingID string, payload []byte, topic, ringStyle string) (*MTResponse, error) {
	if topic == "" {
		topic = IMTTopicRaw
	}
	if ringStyle == "" {
		ringStyle = RingStyleNormal
	}

	params := url.Values{
		"token":     {c.apiKey},
		"thing":     {thingID},
		"message":   {base64.StdEncoding.EncodeToString(payload)},
		"topic":     {topic},
		"ringStyle": {ringStyle},
	}

	apiURL := fmt.Sprintf("%s/Data/DoSendImtMessage?%s", c.apiURL, params.Encode())
	slog.Debug("cloudloop: sending IMT MT", "thing", thingID, "bytes", len(payload), "topic", topic, "ring", ringStyle)

	return c.doPost(ctx, apiURL, "IMT MT")
}

// SendMT sends a Mobile Terminated message using the legacy custom API endpoint.
// Kept for backwards compatibility with existing Hub deployments.
// For new deployments, use SendSBD or SendIMT instead.
func (c *Client) SendMT(ctx context.Context, imei string, payload []byte) (*MTResponse, error) {
	type legacyReq struct {
		IMEI string `json:"imei"`
		Data string `json:"data"`
	}

	reqBody, err := json.Marshal(legacyReq{IMEI: imei, Data: hex.EncodeToString(payload)})
	if err != nil {
		return nil, fmt.Errorf("cloudloop: marshal request: %w", err)
	}

	apiURL := fmt.Sprintf("%s/sbd/mt", c.apiURL)
	slog.Debug("cloudloop: sending MT (legacy)", "imei", imei, "bytes", len(payload))

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	return c.doRequest(req, "legacy MT")
}

// GetDeliveryStatus checks the delivery status of a previously sent MT message.
// Uses the official Cloudloop Data API: Data/GetMtDeliveryStatus.
func (c *Client) GetDeliveryStatus(ctx context.Context, messageID string) (*MTResponse, error) {
	params := url.Values{
		"token":   {c.apiKey},
		"message": {messageID},
	}

	apiURL := fmt.Sprintf("%s/Data/GetMtDeliveryStatus?%s", c.apiURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}

	return c.doRequest(req, "delivery status")
}

// CheckMTStatus checks delivery status using the legacy custom API endpoint.
// For new deployments, use GetDeliveryStatus instead.
func (c *Client) CheckMTStatus(ctx context.Context, mtID string) (*MTResponse, error) {
	apiURL := fmt.Sprintf("%s/sbd/mt/%s", c.apiURL, mtID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	return c.doRequest(req, "legacy MT status")
}

// IsReachable performs a lightweight check to see if the API is reachable.
func (c *Client) IsReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", c.apiURL, nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// doPost creates a POST request with no body and executes it.
func (c *Client) doPost(ctx context.Context, apiURL, label string) (*MTResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create request: %w", err)
	}
	return c.doRequest(req, label)
}

// doRequest executes an HTTP request and parses the Cloudloop JSON response.
func (c *Client) doRequest(req *http.Request, label string) (*MTResponse, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: %s: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: read %s response: %w", label, err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloudloop: %s HTTP %d: %s", label, resp.StatusCode, string(body))
	}

	var mtResp MTResponse
	if err := json.Unmarshal(body, &mtResp); err != nil {
		return nil, fmt.Errorf("cloudloop: parse %s response: %w", label, err)
	}

	slog.Info("cloudloop: "+label+" sent", "id", mtResp.ID, "status", mtResp.Status)
	return &mtResp, nil
}
