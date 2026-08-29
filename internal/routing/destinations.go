package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/webhook"

	hubemail "github.com/meshsat/meshsat-hub/internal/email"
)

// moDecodedPayload is the subset of mo/decoded fields needed for routing dispatch.
type moDecodedPayload struct {
	DeviceGUID string `json:"device_guid,omitempty"`
	IMEI       string `json:"imei,omitempty"`
	Text       string `json:"text"`
	Channel    string `json:"channel"`
}

// NewSMSHandler creates a routing destination handler that sends SMS via Twilio.
// The route's Filter field should contain the recipient phone number(s) (comma-separated, E.164).
// If Filter is empty, the message is logged but not sent.
func NewSMSHandler(client *sms.Client) DestinationHandler {
	return func(ctx context.Context, route *store.Route, deviceID string, payload json.RawMessage) {
		var msg moDecodedPayload
		if err := json.Unmarshal(payload, &msg); err != nil {
			slog.Warn("routing/sms: unmarshal payload failed", "error", err)
			return
		}

		recipients := parseRecipients(route.Filter)
		if len(recipients) == 0 {
			slog.Debug("routing/sms: no recipients in route filter", "route", route.ID)
			return
		}

		text := formatRoutedSMS(deviceID, msg.Text)

		for _, to := range recipients {
			if _, err := client.Send(ctx, to, text); err != nil {
				slog.Error("routing/sms: send failed", "to", to, "device", deviceID, "error", err)
			}
		}
	}
}

// NewEmailHandler creates a routing destination handler that sends email.
// The route's Filter field should contain recipient email address(es) (comma-separated).
func NewEmailHandler(client *hubemail.Client) DestinationHandler {
	return func(ctx context.Context, route *store.Route, deviceID string, payload json.RawMessage) {
		var msg moDecodedPayload
		if err := json.Unmarshal(payload, &msg); err != nil {
			slog.Warn("routing/email: unmarshal payload failed", "error", err)
			return
		}

		recipients := parseRecipients(route.Filter)
		if len(recipients) == 0 {
			slog.Debug("routing/email: no recipients in route filter", "route", route.ID)
			return
		}

		subject := fmt.Sprintf("MeshSat [%s] Message from %s", msg.Channel, deviceID)
		body := fmt.Sprintf("Device: %s\nChannel: %s\n\n%s", deviceID, msg.Channel, msg.Text)

		for _, to := range recipients {
			if err := client.Send(to, subject, body); err != nil {
				slog.Error("routing/email: send failed", "to", to, "device", deviceID, "error", err)
			}
		}
	}
}

// WebhookFirer is the subset of webhook.Dispatcher needed by the routing handler.
type WebhookFirer interface {
	Fire(event webhook.EventType, deviceID string, data json.RawMessage)
}

// NotificationSender can send notifications (Apprise, ntfy, etc.).
// Matches escalation.Notifier interface.
type NotificationSender interface {
	Notify(ctx context.Context, targets []string, subject, body string) error
}

// MQTTPublisher can publish messages to MQTT topics.
// Matches the Publish method of bus.MessageBus.
type MQTTPublisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// NewWebhookHandler creates a routing destination handler that fires outbound webhooks.
// The webhook dispatcher already has its own URL targets configured — this handler
// triggers a "routed_message" event so all registered webhooks receive the message.
func NewWebhookHandler(dispatcher WebhookFirer) DestinationHandler {
	return func(_ context.Context, _ *store.Route, deviceID string, payload json.RawMessage) {
		dispatcher.Fire(webhook.EventType("routed_message"), deviceID, payload)
		slog.Debug("routing/webhook: fired routed_message event", "device", deviceID)
	}
}

// NewNotificationHandler creates a routing destination handler that sends notifications
// via Apprise/ntfy. The route's Filter field can contain Apprise target URLs (comma-separated).
// If Filter is empty, notifications go to the default configured targets.
func NewNotificationHandler(notifier NotificationSender) DestinationHandler {
	return func(ctx context.Context, route *store.Route, deviceID string, payload json.RawMessage) {
		var msg moDecodedPayload
		if err := json.Unmarshal(payload, &msg); err != nil {
			slog.Warn("routing/notification: unmarshal payload failed", "error", err)
			return
		}

		subject := fmt.Sprintf("MeshSat [%s] %s", msg.Channel, deviceID)
		body := msg.Text
		if body == "" {
			body = "(empty message)"
		}

		targets := parseRecipients(route.Filter)
		if err := notifier.Notify(ctx, targets, subject, body); err != nil {
			slog.Error("routing/notification: send failed", "device", deviceID, "error", err)
		}
	}
}

// NewMQTTHandler creates a routing destination handler that republishes messages to
// a fanout MQTT topic. Messages are published to meshsat/routed/{deviceID} so downstream
// consumers (dashboards, external integrations) receive all routed messages.
func NewMQTTHandler(mqtt MQTTPublisher) DestinationHandler {
	return func(_ context.Context, _ *store.Route, deviceID string, payload json.RawMessage) {
		topic := fmt.Sprintf("meshsat/routed/%s", deviceID)
		if err := mqtt.Publish(topic, 1, false, payload); err != nil {
			slog.Error("routing/mqtt: publish failed", "topic", topic, "error", err)
		}
	}
}

// NewTAKHandler creates a routing destination handler that sends CoT events to a TAK server.
// The handler generates a chat-type CoT event from the routed message.
func NewTAKHandler(mqtt MQTTPublisher) DestinationHandler {
	return func(_ context.Context, _ *store.Route, deviceID string, payload json.RawMessage) {
		// Publish to the TAK CoT topic — the TAK subscriber will pick it up
		// and forward to the TAK server as a CoT event.
		topic := fmt.Sprintf("meshsat/%s/tak/cot/out", deviceID)
		if err := mqtt.Publish(topic, 1, false, payload); err != nil {
			slog.Error("routing/tak: publish failed", "topic", topic, "error", err)
		}
	}
}

// NewAPRSHandler creates a routing destination handler that publishes to APRS-IS.
// The handler publishes the message to an APRS MQTT topic for the APRS-IS subscriber to forward.
func NewAPRSHandler(mqtt MQTTPublisher) DestinationHandler {
	return func(_ context.Context, _ *store.Route, deviceID string, payload json.RawMessage) {
		topic := fmt.Sprintf("meshsat/%s/aprs/out", deviceID)
		if err := mqtt.Publish(topic, 1, false, payload); err != nil {
			slog.Error("routing/aprs: publish failed", "topic", topic, "error", err)
		}
	}
}

// parseRecipients splits a comma-separated filter string into individual recipients.
func parseRecipients(filter string) []string {
	if filter == "" {
		return nil
	}
	parts := strings.Split(filter, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// formatRoutedSMS formats a routed message for SMS (160 char limit).
func formatRoutedSMS(deviceID, text string) string {
	prefix := fmt.Sprintf("[%s] ", deviceID)
	msg := prefix + text
	if len(msg) > 160 {
		msg = msg[:157] + "..."
	}
	return msg
}
