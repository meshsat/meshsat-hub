package bridge

import (
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func ns(t string) string {
	if t == "" || t == "default" {
		return "meshsat"
	}
	return "meshsat/" + t
}

func TestRenderNATSUsers(t *testing.T) {
	out := string(RenderNATSUsers([]*store.Bridge{
		{BridgeID: "kit-b", TenantID: "t_x", MQTTUsername: "kit-b", MQTTPasswordHash: "$2a$10$hashB"},
		{BridgeID: "kit-a", TenantID: "default", MQTTUsername: "kit-a", MQTTPasswordHash: "$2a$10$hashA"},
		{BridgeID: "no-creds", TenantID: "default"},
		{BridgeID: "bad", TenantID: "default", MQTTUsername: "bad", MQTTPasswordHash: "plain\"text"},
		{BridgeID: "meshsat", TenantID: "default", MQTTUsername: SharedNATSUser, MQTTPasswordHash: "$2a$10$x"},
	}, ns))
	for _, want := range []string{
		"authorization {",
		"{ user: meshsat, password: $NATS_MQTT_PASSWORD, permissions: { publish: { allow: [\">\"] }, subscribe: { allow: [\">\"] } } }",
		`{ user: "kit-a", password: "$2a$10$hashA", permissions: { publish: { allow: ["meshsat.bridge.kit-a.>", "meshsat.*.position"`,
		`subscribe: { allow: ["meshsat.bridge.kit-a.>", "meshsat.*.mt.>", "meshsat.*.config.>", "meshsat.broadcast.>", "meshsat.hub.>", "$MQTT.sub.>"]`,
		`{ user: "kit-b", password: "$2a$10$hashB", permissions: { publish: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.position"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no-creds") || strings.Contains(out, `"bad"`) || strings.Count(out, "user: meshsat,") != 1 {
		t.Errorf("unexpected users in:\n%s", out)
	}
	if strings.Index(out, `"kit-a"`) > strings.Index(out, `"kit-b"`) {
		t.Error("users must be sorted")
	}
	if string(RenderNATSUsers(nil, ns)) != out[:0]+string(RenderNATSUsers([]*store.Bridge{}, ns)) {
		t.Error("nil and empty must render alike")
	}
}

func TestNATSPermissionsTenantIsolation(t *testing.T) {
	pub, sub := NATSPermissions("meshsat/t_x", "b1")
	for _, s := range append(pub, sub...) {
		if !strings.HasPrefix(s, "meshsat.t_x.") && !strings.HasPrefix(s, "meshsat.broadcast.") && !strings.HasPrefix(s, "meshsat.hub.") && !strings.HasPrefix(s, "$MQTT.") {
			t.Errorf("subject %q escapes the tenant namespace", s)
		}
		if strings.Contains(s, "/") {
			t.Errorf("subject %q must use dots", s)
		}
	}
}
