package sms

import (
	"context"
	"encoding/json"
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
	mqtt   bus.MessageBus
}

// NewSubscriber creates an outbound SMS subscriber.
func NewSubscriber(client *Client, mqtt bus.MessageBus) *Subscriber {
	return &Subscriber{client: client, mqtt: mqtt}
}

// Start subscribes to the outbound SMS topic.
func (s *Subscriber) Start() error {
	return s.mqtt.Subscribe("meshsat/+/mt/sms", 1, s.handle)
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

	result, err := s.client.Send(context.Background(), req.To, req.Body)

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
	statusTopic := "meshsat/" + deviceID + "/mt/sms/status"
	if s.mqtt != nil {
		if err := s.mqtt.PublishJSON(statusTopic, 1, false, status); err != nil {
			slog.Error("sms: mqtt publish status failed", "error", err)
		}
	}
}
