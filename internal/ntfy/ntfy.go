// Package ntfy provides a Go client for the ntfy push notification service.
// ntfy (binwiederhier/ntfy) delivers lightweight push notifications to
// mobile devices and desktops via simple HTTP POST.
//
// API docs: https://docs.ntfy.sh/publish/
package ntfy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Priority levels for ntfy notifications.
const (
	PriorityMin     = "1"
	PriorityLow     = "2"
	PriorityDefault = "3"
	PriorityHigh    = "4"
	PriorityUrgent  = "5"
)

// Client is a Go REST API client for the ntfy push notification service.
type Client struct {
	baseURL    string
	token      string // optional access token for protected topics
	httpClient *http.Client
}

// New creates a new ntfy client.
// baseURL is the ntfy server URL (e.g., "https://ntfy.sh" or "http://ntfy:80").
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// SetToken sets an optional access token for authenticated ntfy topics.
func (c *Client) SetToken(token string) {
	c.token = token
}

// Notify sends a notification to the given topics.
// targets are ntfy topic names (e.g., "meshsat-sos", "meshsat-alerts").
// This implements the escalation.Notifier interface.
func (c *Client) Notify(ctx context.Context, targets []string, subject, body string) error {
	if len(targets) == 0 {
		return nil
	}

	var lastErr error
	for _, topic := range targets {
		if err := c.publish(ctx, topic, subject, body, PriorityUrgent); err != nil {
			slog.Error("ntfy: publish failed", "topic", topic, "error", err)
			lastErr = err
		}
	}
	return lastErr
}

// Publish sends a single notification to a specific topic with a given priority.
func (c *Client) Publish(ctx context.Context, topic, title, message, priority string) error {
	return c.publish(ctx, topic, title, message, priority)
}

func (c *Client) publish(ctx context.Context, topic, title, message, priority string) error {
	url := fmt.Sprintf("%s/%s", c.baseURL, topic)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(message)))
	if err != nil {
		return fmt.Errorf("ntfy: request: %w", err)
	}

	req.Header.Set("Title", title)
	if priority != "" {
		req.Header.Set("Priority", priority)
	}
	req.Header.Set("Tags", "satellite,warning")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d for topic %s", resp.StatusCode, topic)
	}

	slog.Debug("ntfy: notification sent", "topic", topic, "title", title, "priority", priority)
	return nil
}

// Healthz checks if the ntfy service is reachable.
func (c *Client) Healthz(ctx context.Context) error {
	url := c.baseURL + "/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ntfy: health request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy: health: HTTP %d", resp.StatusCode)
	}
	return nil
}
