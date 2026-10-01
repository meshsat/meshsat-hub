package bridge

import (
	"strings"
	"testing"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
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
		// A customer-tenant bridge gets neither the platform's broadcast nor hub.
		// What it does get is its OWN tenant's TAK traffic, named exactly
		// (MESHSAT-1461).
		`{ user: "kit-b", password: "$2a$10$hashB", permissions: { publish: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.position", "meshsat.t_x.*.telemetry", "meshsat.t_x.*.sos", "meshsat.t_x.*.health", "meshsat.t_x.*.signal", "meshsat.t_x.*.mo.>", "meshsat.t_x.*.status.>", "meshsat.t_x.*.sms.>", "meshsat.t_x.*.config.current"] }, subscribe: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.mt.>", "meshsat.t_x.*.config.>", "meshsat.t_x.broadcast.tak.cot.in", "meshsat.t_x.broadcast.tak.cot.in.*", "$MQTT.sub.>"] } }`,
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

// subjectMatches is NATS subject matching: "*" is one token, ">" is the rest.
func subjectMatches(pattern, subject string) bool {
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, tok := range p {
		if tok == ">" {
			return len(s) > i
		}
		if i >= len(s) || (tok != "*" && tok != s[i]) {
			return false
		}
	}
	return len(p) == len(s)
}

func anyMatches(patterns []string, subject string) bool {
	for _, p := range patterns {
		if subjectMatches(p, subject) {
			return true
		}
	}
	return false
}

// CoT export (MESHSAT-1458) rides the bridge's own subtree, so it needs no grant
// of its own, and the broker holds the sender to its own id and its own tenant.
// This pins those three facts, because the Hub's consumer leans on each of them.
func TestABridgeMayExportCoTOnItsOwnSubtreeAndNowhereElse(t *testing.T) {
	for _, c := range []struct{ ns, tenant, otherTenant string }{
		{"meshsat", hubmqtt.DefaultTenant, "t_x"},
		{"meshsat/t_x", "t_x", hubmqtt.DefaultTenant},
	} {
		pub, _ := NATSPermissions(c.ns, "b1")
		subject := func(tenant, bridge string) string {
			return strings.ReplaceAll(hubmqtt.TopicTAKCotOutFor(tenant, bridge), "/", ".")
		}

		if !anyMatches(pub, subject(c.tenant, "b1")) {
			t.Errorf("%s: a bridge may not publish its own CoT export topic %s", c.ns, subject(c.tenant, "b1"))
		}
		if anyMatches(pub, subject(c.tenant, "b2")) {
			t.Errorf("%s: a bridge may publish ANOTHER bridge's export topic; the sender in the "+
				"topic would no longer be the sender", c.ns)
		}
		if anyMatches(pub, subject(c.otherTenant, "b1")) || anyMatches(pub, subject(c.otherTenant, "b2")) {
			t.Errorf("%s: a bridge may publish an export topic in another tenant's namespace", c.ns)
		}

		// The device-shaped topic the clients first shipped with was never
		// publishable, and must not become so: the wide device grants reach that
		// shape in OTHER namespaces.
		n := strings.ReplaceAll(c.ns, "/", ".")
		if anyMatches(pub, n+".b1.tak.cot.out") {
			t.Errorf("%s: a device-shaped tak/cot/out is publishable", c.ns)
		}

		// The one made-up sender a bridge CAN reach: n.*.mo.> with "bridge" in the
		// wildcard. This is why the Hub requires the bridge named in an export
		// topic to be registered to the tenant. If this stops being true, that
		// comment in internal/takhosted/cot_ingest.go wants updating; the check
		// itself should stay.
		if !anyMatches(pub, subject(c.tenant, "mo")) {
			t.Logf("%s: the mo.> grant no longer reaches a bridge-shaped export topic", c.ns)
		}
	}
}

// The return path (MESHSAT-1461): a bridge may read its OWN tenant's TAK
// broadcast, both topics, and nobody else's.
func TestABridgeMaySubscribeOnlyItsOwnTenantsTAKBroadcast(t *testing.T) {
	subject := func(topic string) string { return strings.ReplaceAll(topic, "/", ".") }

	_, cust := NATSPermissions("meshsat/t_x", "b1")
	_, plat := NATSPermissions("meshsat", "kit-a")

	for _, own := range []string{
		subject(hubmqtt.TopicTAKBroadcastFor("t_x")),
		subject(hubmqtt.TopicTAKBroadcastFromFor("t_x", "b2")),
	} {
		if !anyMatches(cust, own) {
			t.Errorf("a customer bridge may not subscribe to its own tenant's %s", own)
		}
		if anyMatches(plat, own) {
			t.Errorf("a PLATFORM bridge may subscribe to a customer's %s", own)
		}
	}
	for _, platforms := range []string{
		subject(hubmqtt.TopicTAKBroadcastFor(hubmqtt.DefaultTenant)),
		subject(hubmqtt.TopicTAKBroadcastFromFor(hubmqtt.DefaultTenant, "kit-b")),
	} {
		if !anyMatches(plat, platforms) {
			t.Errorf("a platform bridge may not subscribe to the platform's %s", platforms)
		}
		if anyMatches(cust, platforms) {
			t.Errorf("a CUSTOMER bridge may subscribe to the platform's %s", platforms)
		}
	}
	// Another customer's, and anything else under its own broadcast.
	for _, no := range []string{
		subject(hubmqtt.TopicTAKBroadcastFor("t_y")),
		subject(hubmqtt.TopicTAKBroadcastFromFor("t_y", "b1")),
		"meshsat.t_x.broadcast.something.else",
		"meshsat.t_x.broadcast.tak.cot.in.b2.deeper",
		"meshsat.t_x.broadcast.tak.cot.out",
	} {
		if anyMatches(cust, no) {
			t.Errorf("a customer bridge may subscribe to %s", no)
		}
	}
}
