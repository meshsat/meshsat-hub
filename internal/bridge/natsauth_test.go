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
		// A platform-tenant bridge keeps broadcast (the operator's TAK picture)
		// but never meshsat.hub.> (MESHSAT-1033).
		`subscribe: { allow: ["meshsat.bridge.kit-a.>", "meshsat.*.mt.>", "meshsat.*.config.>", "meshsat.broadcast.>", "$MQTT.sub.>"]`,
		`{ user: "kit-b", password: "$2a$10$hashB", permissions: { publish: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.position"`,
		// A customer-tenant bridge gets neither broadcast nor hub.
		`{ user: "kit-b", password: "$2a$10$hashB", permissions: { publish: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.position", "meshsat.t_x.*.telemetry", "meshsat.t_x.*.sos", "meshsat.t_x.*.health", "meshsat.t_x.*.signal", "meshsat.t_x.*.mo.>", "meshsat.t_x.*.status.>", "meshsat.t_x.*.sms.>", "meshsat.t_x.*.config.current"] }, subscribe: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.mt.>", "meshsat.t_x.*.config.>", "$MQTT.sub.>"] } }`,
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
		// A customer bridge may only touch its own tenant's namespace and the
		// QoS-1 delivery subject. NOT meshsat.hub.> (the Hub's internal bus,
		// every tenant's SMS and OOB replies) and NOT meshsat.broadcast.> (the
		// platform's TAK picture). MESHSAT-1033.
		if !strings.HasPrefix(s, "meshsat.t_x.") && !strings.HasPrefix(s, "$MQTT.") {
			t.Errorf("customer subject %q escapes the tenant namespace", s)
		}
		if strings.Contains(s, "/") {
			t.Errorf("subject %q must use dots", s)
		}
	}
}

// A customer bridge must not be able to subscribe to the Hub's internal bus or
// the platform broadcast; the platform's own bridge keeps broadcast but still
// never gets hub. This is the whole of MESHSAT-1033.
func TestCustomerBridgeCannotReachHubOrBroadcast(t *testing.T) {
	_, custSub := NATSPermissions("meshsat/t_x", "b1")
	for _, s := range custSub {
		if s == "meshsat.hub.>" || s == "meshsat.broadcast.>" {
			t.Fatalf("a customer bridge may subscribe to %q", s)
		}
	}

	platPub, platSub := NATSPermissions("meshsat", "kit-a")
	if !contains(platSub, "meshsat.broadcast.>") {
		t.Fatal("the platform bridge lost meshsat.broadcast.> (its TAK picture)")
	}
	for _, s := range append(platPub, platSub...) {
		if s == "meshsat.hub.>" {
			t.Fatal("no bridge, not even the platform's, may subscribe to meshsat.hub.>")
		}
	}
	// The platform bridge can still send and receive its own device traffic.
	if !contains(platSub, "meshsat.*.mt.>") || !contains(platPub, "meshsat.*.mo.>") {
		t.Fatal("the platform bridge lost its own device topics")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
