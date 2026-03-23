// Package bridge implements the Hub-side bridge management services.
// Commander sends commands to field bridges via MQTT and waits for responses.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/google/uuid"
)

// CredentialUpdateCommand returns a protocol.Command that tells a bridge to
// update its MQTT credentials. The bridge should reconnect with the new password.
func CredentialUpdateCommand(username, password string) protocol.Command {
	payload, _ := json.Marshal(map[string]string{
		"mqtt_username": username,
		"mqtt_password": password,
	})
	return protocol.Command{
		Cmd:       "update_credentials",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// Commander sends commands to field bridges via MQTT and correlates responses.
type Commander struct {
	mqtt    bus.MessageBus
	store   store.Store
	mu      sync.Mutex
	pending map[string]chan *protocol.CommandResponse // request_id -> response channel
}

// NewCommander creates a new Commander for sending commands to bridges.
func NewCommander(mqtt bus.MessageBus, store store.Store) *Commander {
	return &Commander{
		mqtt:    mqtt,
		store:   store,
		pending: make(map[string]chan *protocol.CommandResponse),
	}
}

// Start subscribes to the bridge command response topic.
func (c *Commander) Start() error {
	if err := c.mqtt.Subscribe(protocol.SubBridgeCmdResp, 1, c.handleResponse); err != nil {
		return fmt.Errorf("commander: subscribe cmd/response: %w", err)
	}
	slog.Info("commander: started, listening for bridge command responses")
	return nil
}

// Stop unsubscribes and closes all pending response channels.
func (c *Commander) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	slog.Info("commander: stopped")
}

// SendCommand publishes a command to a bridge and waits for its response.
// The context controls the overall timeout. If no explicit timeout is set,
// a default of 30s is used (10s for ping).
func (c *Commander) SendCommand(ctx context.Context, bridgeID string, cmd protocol.Command) (*protocol.CommandResponse, error) {
	// Generate request ID if not set.
	if cmd.RequestID == "" {
		cmd.RequestID = uuid.NewString()
	}
	cmd.Protocol = protocol.ProtocolVersion
	cmd.Timestamp = time.Now().UTC()

	// Create response channel.
	respCh := make(chan *protocol.CommandResponse, 1)
	c.mu.Lock()
	c.pending[cmd.RequestID] = respCh
	c.mu.Unlock()

	// Cleanup on exit.
	defer func() {
		c.mu.Lock()
		delete(c.pending, cmd.RequestID)
		c.mu.Unlock()
	}()

	// Publish command to bridge.
	topic := protocol.TopicBridgeCmd(bridgeID)
	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("commander: marshal command: %w", err)
	}
	if err := c.mqtt.Publish(topic, 1, false, payload); err != nil {
		return nil, fmt.Errorf("commander: publish to %s: %w", topic, err)
	}

	slog.Debug("commander: sent command",
		"bridge", bridgeID,
		"cmd", cmd.Cmd,
		"request_id", cmd.RequestID,
	)

	// Apply default timeout if context has no deadline.
	if _, ok := ctx.Deadline(); !ok {
		timeout := 30 * time.Second
		if cmd.Cmd == "ping" {
			timeout = 10 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Wait for response or timeout.
	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, fmt.Errorf("commander: response channel closed")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("commander: timeout waiting for response from bridge %s (request_id=%s)", bridgeID, cmd.RequestID)
	}
}

// handleResponse is the MQTT callback for bridge command responses.
func (c *Commander) handleResponse(topic string, payload []byte) {
	var resp protocol.CommandResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		slog.Debug("commander: invalid response JSON", "error", err, "topic", topic)
		return
	}

	if resp.Protocol != protocol.ProtocolVersion {
		slog.Warn("commander: unknown protocol in response",
			"protocol", resp.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	// Extract bridge ID from topic for logging.
	bridgeID := ""
	// meshsat/bridge/{bridge_id}/cmd/response
	parts := strings.Split(topic, "/")
	if len(parts) >= 3 {
		bridgeID = parts[2]
	}

	c.mu.Lock()
	ch, ok := c.pending[resp.RequestID]
	c.mu.Unlock()

	if !ok {
		slog.Debug("commander: response for unknown request_id (expired or duplicate)",
			"request_id", resp.RequestID,
			"bridge", bridgeID,
			"cmd", resp.Cmd,
		)
		return
	}

	// Non-blocking send — if channel buffer is full, skip (shouldn't happen with buf=1).
	select {
	case ch <- &resp:
		slog.Debug("commander: response received",
			"request_id", resp.RequestID,
			"bridge", bridgeID,
			"cmd", resp.Cmd,
			"status", resp.Status,
		)
	default:
		slog.Warn("commander: duplicate response dropped",
			"request_id", resp.RequestID,
			"bridge", bridgeID,
		)
	}
}
