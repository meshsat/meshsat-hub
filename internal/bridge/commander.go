// Package bridge implements the Hub-side bridge management services.
// Commander sends commands to field bridges via MQTT and waits for responses.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/oob"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
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

// CredentialPushCommand creates a command to push a credential to a bridge.
func CredentialPushCommand(credID, provider, name, credType string, version int, encryptedData []byte, certNotAfter, certFingerprint string) protocol.Command {
	payload, _ := json.Marshal(map[string]interface{}{
		"credential_id":    credID,
		"provider":         provider,
		"name":             name,
		"cred_type":        credType,
		"version":          version,
		"data":             encryptedData, // base64 via json.Marshal
		"cert_not_after":   certNotAfter,
		"cert_fingerprint": certFingerprint,
	})
	return protocol.Command{
		Cmd:       "credential_push",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// CredentialRevokeCommand creates a command to revoke a credential on a bridge.
func CredentialRevokeCommand(credID, reason string) protocol.Command {
	payload, _ := json.Marshal(map[string]string{
		"credential_id": credID,
		"reason":        reason,
	})
	return protocol.Command{
		Cmd:       "credential_revoke",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// DirectoryTrustAnchorRotateCommand creates a command that replaces the
// bridge's pinned directory-signing public key with newPubKey (PKIX DER,
// ECDSA-P256). version identifies the new key so bridges can ignore stale
// or replayed rotates. [MESHSAT-539]
func DirectoryTrustAnchorRotateCommand(newPubKey []byte, version int) protocol.Command {
	payload, _ := json.Marshal(map[string]interface{}{
		"public_key": newPubKey, // base64 via json.Marshal
		"algorithm":  "ecdsa-p256",
		"version":    version,
	})
	return protocol.Command{
		Cmd:       "directory_trust_anchor_rotate",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// DirectoryPushCommand creates a command that ships a signed tenant
// directory snapshot to a bridge. The bridge's directory_push handler
// verifies the ECDSA-P256 signature against the trust anchor it
// pinned at provisioning time (MESHSAT-539) and — on success —
// replaces its local directory_{contacts,addresses,keys,groups,
// dispatch_policy} rows with the snapshot's contents.
//
// The snapshot MUST already be signed by the Hub's directory
// TrustAnchor before it reaches this function; DirectoryPushCommand
// does not resign. Callers in api.DirectoryHandler.GetSnapshot and
// the Hub's change-watcher use api.SignSnapshot to produce the
// signature over api.CanonicalSnapshotBytes(snap) — the bridge
// verifies against the same canonical form. [MESHSAT-540]
func DirectoryPushCommand(snapshot *directory.Snapshot) protocol.Command {
	payload, _ := json.Marshal(snapshot)
	return protocol.Command{
		Cmd:       "directory_push",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// KeyRotateCommand creates a command to push a new encryption key to a bridge or Android device.
// The bridge stores this key in its keystore; the old key is retired with a grace period. [MESHSAT-447]
func KeyRotateCommand(channelType, address, keyHex string, version int) protocol.Command {
	payload, _ := json.Marshal(map[string]interface{}{
		"channel_type": channelType,
		"address":      address,
		"key_hex":      keyHex,
		"version":      version,
	})
	return protocol.Command{
		Cmd:       "key_rotate",
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UTC(),
	}
}

// OOBSender is the out-of-band leg (internal/oob.Service): sealed frames
// over SMS, IMT or SBD for bridges that are MQTT-offline (MESHSAT-964 C).
type OOBSender interface {
	Send(ctx context.Context, tenantID, bridgeID, bearer, cmdName string, args oob.ArgSpec, noReply bool) (*oob.Reply, error)
	ChooseBearer(p *store.OOBPeer, via string, isIMT func(imei string) bool) (string, error)
	Peer(ctx context.Context, tenantID, bridgeID string) (*store.OOBPeer, error)
}

// Commander sends commands to field bridges via MQTT and correlates responses.
type Commander struct {
	mqtt    bus.MessageBus
	store   store.Store
	mu      sync.Mutex
	pending map[string]chan *protocol.CommandResponse // request_id -> response channel
	oob     OOBSender
	isIMT   func(imei string) bool
}

// SetOOB attaches the out-of-band leg; isIMT tells IMT modems from SBD ones.
func (c *Commander) SetOOB(o OOBSender, isIMT func(imei string) bool) {
	c.oob = o
	c.isIMT = isIMT
}

// Via values accepted by SendCommandVia.
const (
	ViaAuto = ""
	ViaMQTT = "mqtt"
)

// SendCommandVia sends a command over the requested leg: "mqtt" (or auto
// while the bridge is online) uses MQTT; "sms", "imt", "sbd" (or auto while
// the bridge is offline and paired) use the out-of-band leg, where only the
// mgmt_* commands, ping, reboot and restart exist. The reply is mapped onto
// CommandResponse: status "ok" for rc 0, else the result code name; Result
// carries the bearer, rc and body.
func (c *Commander) SendCommandVia(ctx context.Context, tenantID, bridgeID string, cmd protocol.Command, via string, online bool) (*protocol.CommandResponse, error) {
	via = strings.ToLower(strings.TrimSpace(via))
	if via == ViaMQTT || (via == ViaAuto && online) || c.oob == nil {
		if via != ViaAuto && via != ViaMQTT {
			return nil, fmt.Errorf("commander: out-of-band leg not configured")
		}
		if !online && via == ViaAuto {
			return nil, fmt.Errorf("commander: bridge is offline and has no out-of-band pairing")
		}
		return c.SendCommand(ctx, bridgeID, cmd)
	}
	peer, err := c.oob.Peer(ctx, tenantID, bridgeID)
	if err != nil {
		return nil, err
	}
	if peer == nil {
		if via == ViaAuto {
			return nil, fmt.Errorf("commander: bridge is offline and not paired for out-of-band commands")
		}
		return nil, oob.ErrNotPaired
	}
	bearer, err := c.oob.ChooseBearer(peer, via, c.isIMT)
	if err != nil {
		return nil, err
	}
	var args oob.ArgSpec
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &args); err != nil {
			return nil, fmt.Errorf("commander: payload for %s: %w", cmd.Cmd, err)
		}
	}
	if cmd.RequestID == "" {
		cmd.RequestID = uuid.NewString()
	}
	reply, err := c.oob.Send(ctx, tenantID, bridgeID, bearer, cmd.Cmd, args, false)
	if err != nil {
		return nil, err
	}
	status := "ok"
	if reply.RC != oob.RCOK {
		status = reply.RC.String()
	}
	result, _ := json.Marshal(map[string]any{"bearer": reply.Bearer, "rc": int(reply.RC), "result": reply.Result, "body": reply.Body, "counter": reply.Counter, "seq": reply.Seq, "total": reply.Total})
	return &protocol.CommandResponse{Protocol: protocol.ProtocolVersion, RequestID: cmd.RequestID, Cmd: cmd.Cmd, Status: status, Result: result, Timestamp: reply.Received}, nil
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
