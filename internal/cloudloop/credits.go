package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// CreditBalance represents the Iridium credit balance from Cloudloop.
type CreditBalance struct {
	Balance   int    `json:"balance"`
	Currency  string `json:"currency,omitempty"`
	Timestamp string `json:"timestamp"`
}

// GetCreditBalance queries the Cloudloop API for the current credit balance.
func (c *Client) GetCreditBalance(ctx context.Context) (*CreditBalance, error) {
	url := fmt.Sprintf("%s/account/balance", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: create balance request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: get balance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cloudloop: read balance response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloudloop: balance HTTP %d: %s", resp.StatusCode, string(body))
	}

	var balance CreditBalance
	if err := json.Unmarshal(body, &balance); err != nil {
		return nil, fmt.Errorf("cloudloop: parse balance: %w", err)
	}

	balance.Timestamp = time.Now().UTC().Format(time.RFC3339)
	return &balance, nil
}

// CreditPoller periodically polls the Cloudloop API for credit balance
// and publishes to the MQTT hub/credits topic.
type CreditPoller struct {
	client   *Client
	bus      bus.MessageBus
	interval time.Duration
}

// NewCreditPoller creates a new credit balance poller.
func NewCreditPoller(client *Client, msgBus bus.MessageBus, interval time.Duration) *CreditPoller {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	return &CreditPoller{
		client:   client,
		bus:      msgBus,
		interval: interval,
	}
}

// Start begins periodic credit balance polling. Blocks until ctx is cancelled.
func (p *CreditPoller) Start(ctx context.Context) {
	slog.Info("credit poller started", "interval", p.interval)

	// Poll immediately on start
	p.poll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("credit poller stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *CreditPoller) poll(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	balance, err := p.client.GetCreditBalance(pollCtx)
	if err != nil {
		slog.Warn("credit poll failed", "error", err)
		return
	}

	slog.Info("credit balance polled", "balance", balance.Balance)

	// Publish to MQTT (retained — always available for new subscribers)
	if err := p.bus.PublishJSON("meshsat/hub/credits", 0, true, balance); err != nil {
		slog.Warn("credit publish failed", "error", err)
	}
}
