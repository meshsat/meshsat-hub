package bridge

import (
	"fmt"
	"strings"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// GeneratePasswordFile produces a Mosquitto password file from bridge credentials.
// Format: username:password_hash (one per line).
// The password_hash should already be in a Mosquitto-compatible format (bcrypt).
// Mosquitto's mosquitto_passwd uses PBKDF2, but the mosquitto-go-auth plugin
// supports bcrypt. For native Mosquitto, use mosquitto_passwd to rehash.
func GeneratePasswordFile(bridges []*store.Bridge) []byte {
	var b strings.Builder
	for _, br := range bridges {
		if br.MQTTUsername == "" || br.MQTTPasswordHash == "" {
			continue
		}
		fmt.Fprintf(&b, "%s:%s\n", br.MQTTUsername, br.MQTTPasswordHash)
	}
	return []byte(b.String())
}

// GenerateACLFile produces a Mosquitto ACL file from bridge records.
// Each bridge gets read/write access to its own subtree and write access
// to shared device telemetry topics.
func GenerateACLFile(bridges []*store.Bridge) []byte {
	var b strings.Builder
	for _, br := range bridges {
		if br.MQTTUsername == "" {
			continue
		}
		fmt.Fprintf(&b, "user %s\n", br.MQTTUsername)
		// Bridge's own namespace (birth, death, health, cmd, cmd/response, device/*)
		fmt.Fprintf(&b, "topic readwrite meshsat/bridge/%s/#\n", br.BridgeID)
		// Device telemetry topics (any device this bridge manages)
		b.WriteString("topic write meshsat/+/position\n")
		b.WriteString("topic write meshsat/+/telemetry\n")
		b.WriteString("topic write meshsat/+/sos\n")
		b.WriteString("topic write meshsat/+/mo/decoded\n")
		b.WriteString("\n")
	}
	return []byte(b.String())
}
