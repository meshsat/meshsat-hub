// Package apprise provides a Go client for the Apprise REST API.
// Apprise (caronc/apprise) is a notification service that supports 90+
// notification backends (Slack, Email, Telegram, SMS, etc.) through a
// single REST API. It runs as a sidecar container in the Hub stack.
//
// API docs: https://github.com/caronc/apprise-api
package apprise

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// Client is a Go REST API client for the Apprise notification service.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a new Apprise client.
// baseURL is the Apprise API base URL (e.g., "http://apprise:8000").
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// notifyRequest is the Apprise /notify/ API request body.
type notifyRequest struct {
	URLs  string `json:"urls"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
	Type  string `json:"type,omitempty"` // info, success, warning, failure
}

// Notify sends a notification to the given targets via Apprise.
// targets are Apprise notification URLs (e.g., "slack://token", "mailto://user:pass@gmail.com").
// This implements the escalation.Notifier interface.
func (c *Client) Notify(ctx context.Context, targets []string, subject, body string) error {
	if len(targets) == 0 {
		return nil
	}

	req := notifyRequest{
		URLs:  strings.Join(targets, ","),
		Title: subject,
		Body:  body,
		Type:  "warning",
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("apprise: marshal: %w", err)
	}

	url := c.baseURL + "/notify/"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("apprise: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("apprise: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("apprise: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	slog.Debug("apprise: notification sent",
		"targets", len(targets), "subject", subject, "status", resp.StatusCode)
	return nil
}

// NotifyStateful sends a notification using a persistent Apprise configuration key.
// The key must be pre-configured in Apprise via POST /add/{key}/.
// This is useful for persistent notification configurations that don't change per-alert.
func (c *Client) NotifyStateful(ctx context.Context, key, subject, body, notifType string) error {
	if notifType == "" {
		notifType = "warning"
	}

	payload := map[string]string{
		"title": subject,
		"body":  body,
		"type":  notifType,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("apprise: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/notify/%s/", c.baseURL, key)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("apprise: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("apprise: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("apprise: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	slog.Debug("apprise: stateful notification sent",
		"key", key, "subject", subject, "status", resp.StatusCode)
	return nil
}

// Healthz checks if the Apprise service is reachable.
func (c *Client) Healthz(ctx context.Context) error {
	url := c.baseURL + "/status/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("apprise: health request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("apprise: health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apprise: health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// AlertData holds the fields available to Go templates for notification formatting.
type AlertData struct {
	AlertID     string
	DeviceIMEI  string
	DeviceLabel string
	Type        string // "sos", "deadman", "geofence", "custom"
	Detail      string
	TierName    string
	TierNum     int // 1-indexed
	TierTotal   int
	Retry       int
	MaxRetries  int
	TriggeredAt string
	Lat         float64
	Lon         float64
}

// defaultSubjectTpl is the Go template for notification subjects.
var defaultSubjectTpl = template.Must(template.New("subject").Parse(
	`[{{.Type}}] {{if .DeviceLabel}}{{.DeviceLabel}}{{else}}{{.DeviceIMEI}}{{end}}: {{.Detail}}`))

// defaultBodyTpl is the Go template for notification bodies.
var defaultBodyTpl = template.Must(template.New("body").Parse(
	`Alert: {{.AlertID}}
Device: {{if .DeviceLabel}}{{.DeviceLabel}} ({{.DeviceIMEI}}){{else}}{{.DeviceIMEI}}{{end}}
Type: {{.Type}}
Tier: {{.TierName}} ({{.TierNum}}/{{.TierTotal}})
Retry: {{.Retry}}/{{.MaxRetries}}
Triggered: {{.TriggeredAt}}
{{- if and .Lat .Lon}}
Location: {{.Lat}}, {{.Lon}}
{{- end}}

{{.Detail}}`))

// FormatSubject renders the alert subject using the default Go template.
func FormatSubject(data AlertData) string {
	var buf bytes.Buffer
	if err := defaultSubjectTpl.Execute(&buf, data); err != nil {
		return fmt.Sprintf("[%s] %s: %s", data.Type, data.DeviceIMEI, data.Detail)
	}
	return buf.String()
}

// FormatBody renders the alert body using the default Go template.
func FormatBody(data AlertData) string {
	var buf bytes.Buffer
	if err := defaultBodyTpl.Execute(&buf, data); err != nil {
		return fmt.Sprintf("Alert %s for device %s: %s", data.AlertID, data.DeviceIMEI, data.Detail)
	}
	return buf.String()
}
