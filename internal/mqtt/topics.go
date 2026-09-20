package mqtt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Device IDs in topic segments (MESHSAT-1022).
//
// A phone number is a device ID on the SMS paths and starts with "+", which
// is an MQTT single-level wildcard. A publish whose topic carries a wildcard
// is refused by the broker: NATS logs "wildcards not allowed in publish's
// topic" and drops the connection, and the client then resends the same
// publish on every reconnect, so one SMS from an E.164 number took a Hub
// replica off the bus for good. Every builder therefore percent-encodes the
// characters MQTT reserves ("+", "#", "/") plus "%" itself, and the parser
// decodes them, so consumers keep seeing the ID they know ("+31653618463")
// while the wire carries "%2B31653618463".
var (
	segmentEncoder = strings.NewReplacer("%", "%25", "+", "%2B", "#", "%23", "/", "%2F")
	segmentDecoder = strings.NewReplacer("%2B", "+", "%23", "#", "%2F", "/", "%25", "%")
)

// EncodeSegment makes an identifier safe as one MQTT topic segment.
func EncodeSegment(id string) string { return segmentEncoder.Replace(id) }

// DecodeSegment reverses EncodeSegment.
func DecodeSegment(seg string) string { return segmentDecoder.Replace(seg) }

// ErrWildcardTopic is returned by a bus that is asked to publish to a topic
// containing "+" or "#": the broker would refuse it and drop the connection.
var ErrWildcardTopic = errors.New("mqtt: publish topic contains a wildcard")

// CheckPublishTopic rejects a publish topic the broker would refuse.
func CheckPublishTopic(topic string) error {
	if strings.ContainsAny(topic, "+#") {
		return fmt.Errorf("%w: %q", ErrWildcardTopic, topic)
	}
	return nil
}

// Topic patterns for the MeshSat Hub MQTT namespace.
// Single-tenant (v0.1): meshsat/{device_id}/...
// Multi-tenant (v0.2+): meshsat/{tenant_id}/{device_id}/...

// TopicMORaw returns the topic for raw MO SBD payloads.
func TopicMORaw(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mo/raw", EncodeSegment(deviceID))
}

// TopicMODecoded returns the topic for decoded MO messages.
func TopicMODecoded(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mo/decoded", EncodeSegment(deviceID))
}

// TopicMTSend returns the topic to publish MT message requests.
func TopicMTSend(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mt/send", EncodeSegment(deviceID))
}

// TopicMTSendWildcard returns the wildcard subscription for all MT send requests.
func TopicMTSendWildcard() string {
	return "meshsat/+/mt/send"
}

// TopicMTStatus returns the topic for MT send results.
func TopicMTStatus(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/mt/status", EncodeSegment(deviceID))
}

// TopicSignal returns the topic for signal quality updates.
func TopicSignal(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/status/signal", EncodeSegment(deviceID))
}

// TopicHealth returns the topic for device health updates.
func TopicHealth(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/status/health", EncodeSegment(deviceID))
}

// TopicPosition returns the topic for GPS position updates.
func TopicPosition(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/position", EncodeSegment(deviceID))
}

// TopicTelemetry returns the topic for telemetry data.
func TopicTelemetry(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/telemetry", EncodeSegment(deviceID))
}

// TopicSOS returns the topic for SOS events.
func TopicSOS(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/sos", EncodeSegment(deviceID))
}

// TopicConfigCurrent returns the topic for current device config.
func TopicConfigCurrent(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/config/current", EncodeSegment(deviceID))
}

// TopicConfigUpdate returns the topic for config update commands.
func TopicConfigUpdate(deviceID string) string {
	return fmt.Sprintf("meshsat/%s/config/update", EncodeSegment(deviceID))
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

// TopicHubCreditsFor is the credit-balance topic of a tenant's Cloudloop
// account: the legacy hub topic for the default tenant,
// meshsat/{tenant}/hub/credits otherwise (MESHSAT-977).
func TopicHubCreditsFor(tenantID string) string {
	if tenantID == "" || tenantID == DefaultTenant {
		return TopicHubCredits()
	}
	return Namespace(tenantID) + "/hub/credits"
}

// TopicAPRSInboundFor is where an APRS-IS message addressed to a tenant's own
// callsign is published: the legacy hub topic for the default tenant,
// meshsat/{tenant}/hub/aprsis/inbound otherwise.
//
// Per tenant since MESHSAT-1121, because each tenant now connects to APRS-IS
// under its own licence. A message addressed to one tenant's callsign arrives on
// that tenant's connection and is nobody else's to read.
func TopicAPRSInboundFor(tenantID string) string {
	if tenantID == "" || tenantID == DefaultTenant {
		return "meshsat/hub/aprsis/inbound"
	}
	return Namespace(tenantID) + "/hub/aprsis/inbound"
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
var reservedSecond = map[string]bool{"hub": true, "broadcast": true, "bridge": true, "relay": true}

// IsHubTopic reports whether topic is one of the Hub's own families
// (meshsat/hub/..., broadcast, bridge, relay) rather than a device topic. A
// wildcard filter such as meshsat/+/sms/inbound matches meshsat/hub/sms/inbound
// too; consumers that only want devices use this to ignore the rest quietly.
func IsHubTopic(topic string) bool {
	parts := strings.Split(topic, "/")
	return len(parts) >= 2 && parts[0] == "meshsat" && reservedSecond[parts[1]]
}

// WebSocket relay topics (MESHSAT-612): {namespace}/relay/{bridge}/{client}/{up|down}.
// A rendezvous between the replica holding the bridge socket and the one
// holding the client socket; never retained, never stored. Both ids come from
// authenticated callers but are still percent-encoded: a "+" in either would
// take the publishing replica off the bus (MESHSAT-1022).

// RelayTopic builds the topic for one direction of one tunnel.
func RelayTopic(tenantID, bridgeID, clientID, dir string) string {
	return Namespace(tenantID) + "/relay/" + EncodeSegment(bridgeID) + "/" + EncodeSegment(clientID) + "/" + dir
}

// RelayFilters returns the two subscription filters (legacy and tenant
// shape) that see every tunnel's traffic in one direction.
func RelayFilters(dir string) []string {
	return []string{"meshsat/relay/+/+/" + dir, "meshsat/+/relay/+/+/" + dir}
}

// ParseRelayTopic splits a relay topic of either shape. ok is false for
// anything else, including a direction other than up or down.
func ParseRelayTopic(topic string) (tenantID, bridgeID, clientID, dir string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 5 || parts[0] != "meshsat" {
		return "", "", "", "", false
	}
	switch {
	case len(parts) == 5 && parts[1] == "relay":
		tenantID = DefaultTenant
		parts = parts[2:]
	case len(parts) == 6 && parts[2] == "relay" && !reservedSecond[parts[1]] && parts[1] != "":
		tenantID = parts[1]
		parts = parts[3:]
	default:
		return "", "", "", "", false
	}
	if parts[2] != "up" && parts[2] != "down" || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", false
	}
	return tenantID, DecodeSegment(parts[0]), DecodeSegment(parts[1]), parts[2], true
}

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
	return Namespace(tenantID) + "/" + EncodeSegment(deviceID) + "/" + suffix
}

// BridgeTopic builds {namespace}/bridge/{bridgeID}/{suffix} for the tenant.
func BridgeTopic(tenantID, bridgeID, suffix string) string {
	return Namespace(tenantID) + "/bridge/" + EncodeSegment(bridgeID) + "/" + suffix
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

// MOAck is the Hub's receipt for one MO, sent to the bridge that owns the modem
// (MESHSAT-1246). The phone matches it to the message it sent by IMEI and MOMSN,
// which +SBDIX reported to it for the same session, and shows a second tick.
//
// It lives beside its topic rather than in a provider package because two
// providers now send it -- Rock7 and Cloudloop (MESHSAT-1257) -- and the phone
// parses one shape. A second copy of these four fields is a drift waiting to
// happen.
type MOAck struct {
	IMEI       string `json:"imei"`
	MOMSN      int    `json:"momsn"`
	Bearer     string `json:"bearer"`
	ReceivedAt string `json:"received_at"`
}

// TopicBridgeMOAckFor is where the Hub tells a bridge it has an MO from one of
// the bridge's modems (MESHSAT-1246). It sits under the bridge's own topic
// root, which that bridge's NATS user may already subscribe to.
func TopicBridgeMOAckFor(tenantID, bridgeID string) string {
	return BridgeTopic(tenantID, bridgeID, "mo/ack")
}
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
		return DefaultTenant, DecodeSegment(parts[1]), strings.Join(parts[2:], "/"), true
	}
	if len(parts) >= 4 && deviceSuffixHeads[parts[3]] && !reservedSecond[parts[1]] && !reservedSecond[parts[2]] && parts[2] != "" {
		return parts[1], DecodeSegment(parts[2]), strings.Join(parts[3:], "/"), true
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
		return DefaultTenant, DecodeSegment(parts[2]), parts[3:], parts[2] != ""
	}
	if len(parts) >= 5 && parts[2] == "bridge" && !reservedSecond[parts[1]] {
		return parts[1], DecodeSegment(parts[3]), parts[4:], parts[3] != ""
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

// MessageDigest is the stable identity of one MQTT message: a digest of the
// topic and the payload bytes exactly as they arrived.
//
// Both Hub replicas receive every message (plain Subscribe, not queue groups),
// so this is what lets them agree on which message they are looking at without
// talking to each other. A clock or a locally generated id cannot do that --
// they differ between the two processes. Used as a row id so a second insert
// collides harmlessly, and as a claim key so exactly one replica acts.
func MessageDigest(topic string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(topic))
	h.Write([]byte{0})
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// FallbackMessageID derives a stable message ID from a topic and payload for
// publishers that carry no "id" field. Two replicas receiving the same MQTT
// message derive the same ID, so the second insert is a duplicate no-op.
func FallbackMessageID(topic string, payload []byte) string {
	return "mo-" + MessageDigest(topic, payload)
}

// FallbackPositionID is the same identity wearing a position's prefix, so a row
// id says what kind of thing it is (MESHSAT-1120).
func FallbackPositionID(topic string, payload []byte) string {
	return "pos-" + MessageDigest(topic, payload)
}
