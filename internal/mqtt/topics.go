package mqtt

import (
	"crypto/sha256"
	"encoding/hex"
)

import (
	"fmt"
	"strings"
)

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

// Tenant-prefixed namespace (MESHSAT-864 MR 20).
//
// Devices and bridges of the default tenant keep the historical topics
// (meshsat/{device}/..., meshsat/bridge/{id}/...). Every other tenant lives
// under meshsat/{tenant}/{device}/... and meshsat/{tenant}/bridge/{id}/...;
// the Hub subscribes to both shapes and publishes on the shape that belongs
// to the device's tenant. The second segment of a legacy topic is a device
// ID, a bridge marker or a reserved word, never one of the known suffix
// heads, which is what tells the two shapes apart.

// DefaultTenant is the tenant whose topics carry no prefix.
const DefaultTenant = "default"

// deviceSuffixHeads are the first segments that can follow a device ID.
var deviceSuffixHeads = map[string]bool{
	"mo": true, "mt": true, "position": true, "sos": true, "status": true,
	"telemetry": true, "config": true, "sms": true, "health": true, "signal": true,
}

// reservedSecond are second segments that are neither devices nor tenants.
var reservedSecond = map[string]bool{"hub": true, "broadcast": true, "bridge": true}

// Namespace returns the topic root for a tenant: "meshsat" for the default
// tenant, "meshsat/{tenant}" otherwise. Bridges receive it as
// mqtt_topic_prefix in the provisioning bundle.
func Namespace(tenantID string) string {
	if tenantID == "" || tenantID == DefaultTenant {
		return "meshsat"
	}
	return "meshsat/" + tenantID
}

// DeviceTopic builds {namespace}/{device}/{suffix} for the tenant.
func DeviceTopic(tenantID, deviceID, suffix string) string {
	return Namespace(tenantID) + "/" + deviceID + "/" + suffix
}

// BridgeTopic builds {namespace}/bridge/{bridgeID}/{suffix} for the tenant.
func BridgeTopic(tenantID, bridgeID, suffix string) string {
	return Namespace(tenantID) + "/bridge/" + bridgeID + "/" + suffix
}

// Tenant-aware variants of the builders above.
func TopicMORawFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "mo/raw")
}
func TopicMODecodedFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "mo/decoded")
}
func TopicMTSendFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "mt/send")
}
func TopicMTStatusFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "mt/status")
}
func TopicPositionFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "position")
}
func TopicSOSFor(tenantID, deviceID string) string { return DeviceTopic(tenantID, deviceID, "sos") }
func TopicConfigCurrentFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "config/current")
}
func TopicConfigUpdateFor(tenantID, deviceID string) string {
	return DeviceTopic(tenantID, deviceID, "config/update")
}

// DualFilters returns the subscription filters for both topic shapes of a
// legacy filter: "meshsat/+/mo/decoded" -> itself and "meshsat/+/+/mo/decoded";
// "meshsat/bridge/+/birth" -> itself and "meshsat/+/bridge/+/birth".
func DualFilters(legacy string) []string {
	const prefix = "meshsat/"
	if !strings.HasPrefix(legacy, prefix) {
		return []string{legacy}
	}
	return []string{legacy, prefix + "+/" + legacy[len(prefix):]}
}

// ParseDeviceTopic splits a device topic of either shape into tenant, device
// and suffix. ok is false for hub/broadcast/bridge topics and anything that
// is not a device topic.
func ParseDeviceTopic(topic string) (tenantID, deviceID, suffix string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 3 || parts[0] != "meshsat" || parts[1] == "" {
		return "", "", "", false
	}
	if deviceSuffixHeads[parts[2]] && !reservedSecond[parts[1]] {
		return DefaultTenant, parts[1], strings.Join(parts[2:], "/"), true
	}
	if len(parts) >= 4 && deviceSuffixHeads[parts[3]] && !reservedSecond[parts[1]] && !reservedSecond[parts[2]] && parts[2] != "" {
		return parts[1], parts[2], strings.Join(parts[3:], "/"), true
	}
	return "", "", "", false
}

// ParseBridgeTopic splits meshsat/bridge/{id}/... or meshsat/{tenant}/bridge/{id}/...
// into tenant, bridge ID and the remaining segments.
func ParseBridgeTopic(topic string) (tenantID, bridgeID string, rest []string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[0] != "meshsat" {
		return "", "", nil, false
	}
	if parts[1] == "bridge" {
		return DefaultTenant, parts[2], parts[3:], parts[2] != ""
	}
	if len(parts) >= 5 && parts[2] == "bridge" && !reservedSecond[parts[1]] {
		return parts[1], parts[3], parts[4:], parts[3] != ""
	}
	return "", "", nil, false
}

// ExtractDeviceID returns the device ID of a device topic of either shape,
// or "" when the topic is not a device topic.
func ExtractDeviceID(topic string) string {
	_, device, _, ok := ParseDeviceTopic(topic)
	if !ok {
		return ""
	}
	return device
}

// ExtractTenantID returns the tenant a device topic belongs to ("" when the
// topic is not a device topic; DefaultTenant for the legacy shape).
func ExtractTenantID(topic string) string {
	tenant, _, _, ok := ParseDeviceTopic(topic)
	if !ok {
		return ""
	}
	return tenant
}

// FallbackMessageID derives a stable message ID from a topic and payload for
// publishers that carry no "id" field. Two replicas receiving the same MQTT
// message derive the same ID, so the second insert is a duplicate no-op.
func FallbackMessageID(topic string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(topic))
	h.Write([]byte{0})
	h.Write(payload)
	return "mo-" + hex.EncodeToString(h.Sum(nil))[:16]
}
