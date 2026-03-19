package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cubeos-app/meshsat-hub/internal/sms"
	"github.com/cubeos-app/meshsat-hub/internal/store"

	hubemail "github.com/cubeos-app/meshsat-hub/internal/email"
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
