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
		`{ user: "kit-b", password: "$2a$10$hashB", permissions: { publish: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.position", "meshsat.t_x.*.telemetry", "meshsat.t_x.*.sos", "meshsat.t_x.*.health", "meshsat.t_x.*.signal", "meshsat.t_x.*.mo.>", "meshsat.t_x.*.status.>", "meshsat.t_x.*.sms.>", "meshsat.t_x.*.config.current"], deny: ["meshsat.t_x.bridge.*.msg.in", "meshsat.t_x.bridge.*.msg.in.>", "meshsat.t_x.bridge.*.msg.status", "meshsat.t_x.bridge.*.msg.status.>", "meshsat.t_x.bridge.*.sos.ack", "meshsat.t_x.bridge.*.sos.ack.>", "meshsat.t_x.bridge.*.hub.caps", "meshsat.t_x.bridge.*.hub.caps.>", "meshsat.t_x.bridge.*.tak.cot.in", "meshsat.t_x.bridge.*.tak.cot.in.>", "meshsat.t_x.bridge.*.rns.access", "meshsat.t_x.bridge.*.rns.access.>", "meshsat.t_x.bridge.*.warn.in", "meshsat.t_x.bridge.*.warn.in.>"] }, subscribe: { allow: ["meshsat.t_x.bridge.kit-b.>", "meshsat.t_x.*.mt.>", "meshsat.t_x.*.config.>", "meshsat.t_x.broadcast.tak.cot.in", "meshsat.t_x.broadcast.tak.cot.in.*", "$MQTT.sub.>"] } }`,
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

// MESHSAT-1706: a bridge reads the chats the Hub routes to its crews on its own
// tak/cot/in and never publishes there; it still exports on tak/cot/out, and
// another bridge's tak/cot/in is out of its reach.
func TestNATSPermissionsTheHubOwnsTAKCotIn(t *testing.T) {
	for _, c := range []struct{ ns, root string }{{"meshsat", "meshsat"}, {"meshsat/t_x", "meshsat.t_x"}} {
		pub, sub := NATSPermissions(c.ns, "b1")
		deny := NATSPublishDeny(c.ns, "b1")
		own := c.root + ".bridge.b1.tak.cot."
		for subject, wantPub := range map[string]bool{
			own + "out": true, own + "in": false, own + "in.x": false,
		} {
			if got := natsAllows(pub, subject) && !natsAllows(deny, subject); got != wantPub {
				t.Errorf("%s: publish %s = %v, want %v", c.ns, subject, got, wantPub)
			}
			if !natsAllows(sub, subject) {
				t.Errorf("%s: cannot subscribe to its own %s", c.ns, subject)
			}
		}
		other := c.root + ".bridge.b2.tak.cot.in"
		if natsAllows(pub, other) || natsAllows(sub, other) {
			t.Errorf("%s: reaches another bridge's tak/cot/in", c.ns)
		}
	}
}

// C4.1 (MESHSAT-1584): a client publishes its own msg/out and msg/ack and never
// its msg/in or msg/status, which only the Hub publishes; it still subscribes to
// all four of its own. Platform and customer tenants alike.
func TestNATSPermissionsTheHubOwnsMsgInAndStatus(t *testing.T) {
	for _, c := range []struct{ ns, root string }{{"meshsat", "meshsat"}, {"meshsat/t_x", "meshsat.t_x"}} {
		pub, sub := NATSPermissions(c.ns, "b1")
		deny := NATSPublishDeny(c.ns, "b1")
		own := c.root + ".bridge.b1.msg."
		for subject, wantPub := range map[string]bool{
			own + "out": true, own + "ack": true, own + "in": false, own + "status": false, own + "status.x": false, own + "in.x": false,
		} {
			if got := natsAllows(pub, subject) && !natsAllows(deny, subject); got != wantPub {
				t.Errorf("%s: publish %s = %v, want %v", c.ns, subject, got, wantPub)
			}
			if !natsAllows(sub, subject) {
				t.Errorf("%s: cannot subscribe to its own %s", c.ns, subject)
			}
		}
		// Another bridge's msg/in is neither publishable nor readable.
		other := c.root + ".bridge.b2.msg.in"
		if natsAllows(pub, other) || natsAllows(sub, other) {
			t.Errorf("%s: reaches another bridge's msg/in", c.ns)
		}
	}
}

// natsAllows matches a subject against NATS subject patterns (* one token, >
// the rest), as nats-server does for permissions.
func natsAllows(patterns []string, subject string) bool {
	st := strings.Split(subject, ".")
	for _, p := range patterns {
		pt := strings.Split(p, ".")
		ok := true
		for i, tok := range pt {
			if tok == ">" {
				ok = len(st) > i
				break
			}
			if i >= len(st) || (tok != "*" && tok != st[i]) {
				ok = false
				break
			}
			if i == len(pt)-1 && len(st) != len(pt) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The Hub's own copy of a satellite MO (hubmqtt.TopicSatMOFor) is what the
// "@" lane sends on, BECAUSE no bridge can publish it (MESHSAT-1685): every
// bridge may publish mo/decoded for any device id in its namespace. If a
// grant ever reaches it, a kit can make its tenant's Twilio text anyone.
func TestNoBridgeMayPublishTheHubsSatelliteCopy(t *testing.T) {
	for _, ns := range []string{"meshsat", "meshsat/t_alpha"} {
		publish, _ := NATSPermissions(ns, "kit-a")
		for _, tenant := range []string{"default", "t_alpha", "t_other"} {
			subject := strings.ReplaceAll(hubmqtt.TopicSatMOFor(tenant, "300434060000101"), "/", ".")
			for _, p := range publish {
				if subjectMatches(p, subject) {
					t.Errorf("a bridge in %s may publish %s through grant %s", ns, subject, p)
				}
			}
		}
	}
}

// MESHSAT-1715: rns/access carries the tenant's Reticulum access code and only
// the Hub publishes it. The device grants ({ns}.*.mo.>, .status.>, .sms.>)
// also match the subtree of a bridge whose id is mo, status or sms, so the
// deny covers every bridge of the tenant: no bridge can publish any of the
// Hub's answers to another, whatever that one is called. The kit still asks
// on its own rns/join and reads its own rns/access.
func TestNATSPermissionsNoBridgePublishesTheHubsAnswersToAnother(t *testing.T) {
	hubOnly := []string{"msg.in", "msg.status", "sos.ack", "hub.caps", "tak.cot.in", "rns.access"}
	for _, c := range []struct{ ns, root string }{{"meshsat", "meshsat"}, {"meshsat/t_x", "meshsat.t_x"}} {
		pub, sub := NATSPermissions(c.ns, "b1")
		deny := NATSPublishDeny(c.ns, "b1")
		for _, target := range []string{"b1", "mo", "status", "sms", "b2"} {
			for _, topic := range hubOnly {
				for _, subject := range []string{c.root + ".bridge." + target + "." + topic, c.root + ".bridge." + target + "." + topic + ".x"} {
					if natsAllows(pub, subject) && !natsAllows(deny, subject) {
						t.Errorf("%s: b1 may publish %s", c.ns, subject)
					}
				}
			}
		}
		own := c.root + ".bridge.b1.rns."
		if !natsAllows(pub, own+"join") || natsAllows(deny, own+"join") {
			t.Errorf("%s: b1 cannot ask on its own rns/join", c.ns)
		}
		if !natsAllows(sub, own+"access") {
			t.Errorf("%s: b1 cannot read its own rns/access", c.ns)
		}
	}
}

// A '.' in a bridge id splits its grant into two subject tokens, and the
// broker's MQTT layer maps a '.' in a topic to "//", so a bridge called
// x.tak would be granted bridge x's tak subtree, not its own (MESHSAT-1731).
// Such an id is never rendered.
func TestNATSUsersNeverGrantADottedBridgeID(t *testing.T) {
	pub, sub := NATSPermissions("meshsat", "x.tak")
	// Bridge x's CoT export and its crews' chats, but not its own topics
	// (meshsat/bridge/x.tak/birth arrives as meshsat.bridge.x//tak.birth).
	if !natsAllows(pub, "meshsat.bridge.x.tak.cot.out") || !natsAllows(sub, "meshsat.bridge.x.tak.cot.in") || natsAllows(pub, "meshsat.bridge.x//tak.birth") {
		t.Fatal("the hazard this guards against is not what the grant shows")
	}
	out := string(RenderNATSUsers([]*store.Bridge{
		{BridgeID: "x", TenantID: "default", MQTTUsername: "x", MQTTPasswordHash: "$2a$10$hashX"},
		{BridgeID: "x.tak", TenantID: "default", MQTTUsername: "xtak", MQTTPasswordHash: "$2a$10$hashY"},
		{BridgeID: "x.msg", TenantID: "t_x", MQTTUsername: "x.msg", MQTTPasswordHash: "$2a$10$hashZ"},
	}, ns))
	if strings.Contains(out, "xtak") || strings.Contains(out, "x.msg") || !strings.Contains(out, `user: "x"`) {
		t.Fatalf("a dotted bridge id was rendered, or x was not:\n%s", out)
	}
}

func TestValidBridgeID(t *testing.T) {
	for id, want := range map[string]bool{
		"kit-01": true, "parallax": true, "android_3f2a": true, strings.Repeat("a", 128): true,
		"": false, "x.tak": false, ".": false, "a b": false, "x/y": false, "x+y": false, "x#": false,
		"x*": false, "x>": false, `x"`: false, "%2E": false, strings.Repeat("a", 129): false,
	} {
		if got := ValidBridgeID(id); got != want {
			t.Errorf("ValidBridgeID(%q) = %v, want %v", id, got, want)
		}
	}
}
