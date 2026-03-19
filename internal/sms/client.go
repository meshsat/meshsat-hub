// Package sms provides SMS send/receive via Twilio or Vonage REST APIs.
// Outbound: subscribe to meshsat/+/mt/sms on MQTT, send via provider.
// Inbound: POST /api/webhook/sms receives provider callbacks, publishes to MQTT.
package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client sends SMS messages via Twilio REST API.
type Client struct {
	accountSID string
	authToken  string
	fromNumber string
	apiURL     string // overridable for tests
	httpClient *http.Client
}

// SendResult is the response after queuing an outbound SMS.
type SendResult struct {
	SID    string `json:"sid"`
	Status string `json:"status"` // "queued", "sent", "delivered", "failed", "undelivered"
	Error  string `json:"error,omitempty"`
}

// NewClient creates a Twilio SMS client using Account SID + Auth Token.
func NewClient(accountSID, authToken, fromNumber string) *Client {
	return &Client{
		accountSID: accountSID,
		authToken:  authToken,
		fromNumber: fromNumber,
		apiURL:     fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s", accountSID),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithAPIKey creates a Twilio SMS client using API Key auth.
// accountSID is used in the URL path; apiKeySID + apiKeySecret for Basic Auth.
func NewClientWithAPIKey(accountSID, apiKeySID, apiKeySecret, fromNumber string) *Client {
	return &Client{
		accountSID: apiKeySID,    // used for Basic Auth username
		authToken:  apiKeySecret, // used for Basic Auth password
		fromNumber: fromNumber,
		apiURL:     fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s", accountSID),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetAPIURL overrides the API base URL (for testing with mock server).
func (c *Client) SetAPIURL(url string) {
	c.apiURL = url
}

// Send sends an SMS message to the given phone number.
func (c *Client) Send(ctx context.Context, to, body string) (*SendResult, error) {
	if to == "" {
		return nil, fmt.Errorf("sms: empty recipient number")
	}
	if body == "" {
		return nil, fmt.Errorf("sms: empty message body")
	}

	form := url.Values{
		"To":   {to},
		"From": {c.fromNumber},
		"Body": {body},
	}

	apiURL := fmt.Sprintf("%s/Messages.json", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("sms: create request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sms: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sms: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("sms: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("sms: parse response: %w", err)
	}

	slog.Info("sms: message sent", "to", to, "sid", result.SID, "status", result.Status)
	return &SendResult{SID: result.SID, Status: result.Status}, nil
}

// CheckStatus queries the delivery status of a previously sent message.
func (c *Client) CheckStatus(ctx context.Context, messageSID string) (*SendResult, error) {
	apiURL := fmt.Sprintf("%s/Messages/%s.json", c.apiURL, messageSID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sms: create request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sms: check status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sms: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("sms: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("sms: parse response: %w", err)
	}

	return &SendResult{SID: result.SID, Status: result.Status}, nil
}
