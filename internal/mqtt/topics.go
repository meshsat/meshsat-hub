package mqtt

import "fmt"

// Topic patterns for the MeshSat Hub MQTT namespace.
// Single-tenant (v0.1): meshsat/{device_id}/...
// Multi-tenant (v0.2+): meshsat/{tenant_id}/{device_id}/...

// TopicMORaw returns the topic for raw MO SBD payloads.
func TopicMORaw(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mo/raw", deviceID)
}

// TopicMODecoded returns the topic for decoded MO messages.
func TopicMODecoded(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mo/decoded", deviceID)
}

// TopicMTSend returns the topic to publish MT message requests.
func TopicMTSend(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mt/send", deviceID)
}

// TopicMTSendWildcard returns the wildcard subscription for all MT send requests.
func TopicMTSendWildcard() string {
	return "meshsat/+/mt/send"
}

// TopicMTStatus returns the topic for MT send results.
func TopicMTStatus(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mt/status", deviceID)
}

// TopicSignal returns the topic for signal quality updates.
func TopicSignal(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/status/signal", deviceID)
}

// TopicHealth returns the topic for device health updates.
func TopicHealth(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/status/health", deviceID)
}

// TopicPosition returns the topic for GPS position updates.
func TopicPosition(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/position", deviceID)
}

// TopicTelemetry returns the topic for telemetry data.
func TopicTelemetry(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/telemetry", deviceID)
}

// TopicSOS returns the topic for SOS events.
func TopicSOS(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/sos", deviceID)
}

// TopicConfigCurrent returns the topic for current device config.
func TopicConfigCurrent(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/config/current", deviceID)
}

// TopicConfigUpdate returns the topic for config update commands.
func TopicConfigUpdate(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/config/update", deviceID)
}

// TopicHubStatus returns the hub health status topic.
func TopicHubStatus() string {
	return "meshsat/hub/status"
}

// TopicHubEvents returns the hub system events topic.
func TopicHubEvents() string {
	return "meshsat/hub/events"
}

// TopicHubCredits returns the Iridium credit balance topic.
func TopicHubCredits() string {
	return "meshsat/hub/credits"
}

// ExtractDeviceID extracts the device ID from a topic matching meshsat/+/mt/send.
// Returns empty string if the topic doesn't match the expected pattern.
func ExtractDeviceID(topic string) string {
	// Expected: meshsat/{deviceID}/mt/send
	const prefix = "meshsat/"
	if len(topic) < len(prefix)+2 || topic[:len(prefix)] != prefix {
		return ""
	}
	rest := topic[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i]
		}
	}
	return ""
}
