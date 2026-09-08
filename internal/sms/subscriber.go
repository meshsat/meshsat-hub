package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// OutboundRequest is the JSON payload on meshsat/+/mt/sms to trigger an outbound SMS.
type OutboundRequest struct {
	To   string `json:"to"`   // E.164 phone number
	Body string `json:"body"` // message text
}

// OutboundStatus is published to meshsat/{device}/mt/sms/status after send attempt.
type OutboundStatus struct {
	SID    string `json:"sid,omitempty"`
	To     string `json:"to"`
	Status string `json:"status"` // "queued", "sent", "failed"
	Error  string `json:"error,omitempty"`
}

// Subscriber listens on meshsat/+/mt/sms and sends outbound SMS via the client.
type Subscriber struct {
	client *Client
	pool   *ClientPool // per-tenant accounts (MESHSAT-977)
	mqtt   bus.MessageBus
}

// SetClientPool makes sends use the topic tenant's Twilio account.
func (s *Subscriber) SetClientPool(p *ClientPool) { s.pool = p }

// NewSubscriber creates an outbound SMS subscriber.
func NewSubscriber(client *Client, mqtt bus.MessageBus) *Subscriber {
	return &Subscriber{client: client, mqtt: mqtt}
}

// Start subscribes to the outbound SMS topic.
func (s *Subscriber) Start() error {
	for _, f := range hubmqtt.DualFilters("meshsat/+/mt/sms") {
		if err := s.mqtt.Subscribe(f, 1, s.handle); err != nil {
			return err
		}
	}
	return nil
}

func (s *Subscriber) handle(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)

	var req OutboundRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		slog.Warn("sms: invalid outbound request", "error", err, "device", deviceID)
		return
	}

	if req.To == "" || req.Body == "" {
		slog.Warn("sms: missing to or body in outbound request", "device", deviceID)
		return
	}

	slog.Info("sms: sending outbound", "device", deviceID, "to", req.To)

	tenantID := hubmqtt.ExtractTenantID(topic)
	client := s.client
	if s.pool != nil {
		client = s.pool.ForTenant(context.Background(), tenantID)
	}
	var result *SendResult
	err := fmt.Errorf("no Twilio account configured for tenant %s", tenantID)
	if client != nil {
		result, err = client.Send(context.Background(), req.To, req.Body)
	}

	status := OutboundStatus{To: req.To}
	if err != nil {
		slog.Error("sms: send failed", "error", err, "device", deviceID, "to", req.To)
		status.Status = "failed"
		status.Error = err.Error()
	} else {
		status.SID = result.SID
		status.Status = result.Status
	}

	// Publish status to MQTT.
	statusTopic := hubmqtt.DeviceTopic(hubmqtt.ExtractTenantID(topic), deviceID, "mt/sms/status")
	if s.mqtt != nil {
		if err := s.mqtt.PublishJSON(statusTopic, 1, false, status); err != nil {
			slog.Error("sms: mqtt publish status failed", "error", err)
		}
	}
}
