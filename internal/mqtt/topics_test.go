package mqtt

import (
	"reflect"
	"testing"
)

func TestParseDeviceTopic(t *testing.T) {
	cases := []struct {
		topic                  string
		tenant, device, suffix string
		ok                     bool
	}{
		{"meshsat/300234063904190/mo/decoded", "default", "300234063904190", "mo/decoded", true},
		{"meshsat/300234063904190/position", "default", "300234063904190", "position", true},
		{"meshsat/300234063904190/mt/sms/status", "default", "300234063904190", "mt/sms/status", true},
		{"meshsat/t_2ca6/300234063904190/mo/decoded", "t_2ca6", "300234063904190", "mo/decoded", true},
		{"meshsat/t_2ca6/300234063904190/position", "t_2ca6", "300234063904190", "position", true},
		{"meshsat/hub/status", "", "", "", false},
		{"meshsat/broadcast/tak/cot/in", "", "", "", false},
		{"meshsat/bridge/b1/birth", "", "", "", false},
		{"meshsat/t_2ca6/bridge/b1/birth", "", "", "", false},
		{"other/x/position", "", "", "", false},
		{"meshsat", "", "", "", false},
	}
	for _, c := range cases {
		tenant, device, suffix, ok := ParseDeviceTopic(c.topic)
		if ok != c.ok || tenant != c.tenant || device != c.device || suffix != c.suffix {
			t.Errorf("%s: got (%q,%q,%q,%v) want (%q,%q,%q,%v)", c.topic, tenant, device, suffix, ok, c.tenant, c.device, c.suffix, c.ok)
		}
		if got := ExtractDeviceID(c.topic); got != c.device {
			t.Errorf("ExtractDeviceID(%s) = %q", c.topic, got)
		}
	}
}

func TestParseBridgeTopic(t *testing.T) {
	tenant, id, rest, ok := ParseBridgeTopic("meshsat/bridge/b1/device/300/birth")
	if !ok || tenant != "default" || id != "b1" || !reflect.DeepEqual(rest, []string{"device", "300", "birth"}) {
		t.Errorf("legacy: %q %q %v %v", tenant, id, rest, ok)
	}
	tenant, id, rest, ok = ParseBridgeTopic("meshsat/t_x/bridge/b1/health")
	if !ok || tenant != "t_x" || id != "b1" || !reflect.DeepEqual(rest, []string{"health"}) {
		t.Errorf("tenant: %q %q %v %v", tenant, id, rest, ok)
	}
	if _, _, _, ok := ParseBridgeTopic("meshsat/300/position"); ok {
		t.Error("device topic must not parse as bridge topic")
	}
}

func TestBuildersAndFilters(t *testing.T) {
	if Namespace("") != "meshsat" || Namespace("default") != "meshsat" || Namespace("t_x") != "meshsat/t_x" {
		t.Error("Namespace")
	}
	if TopicMODecodedFor("default", "1") != "meshsat/1/mo/decoded" || TopicMODecodedFor("t_x", "1") != "meshsat/t_x/1/mo/decoded" {
		t.Error("TopicMODecodedFor")
	}
	if BridgeTopic("t_x", "b1", "config/hemb") != "meshsat/t_x/bridge/b1/config/hemb" {
		t.Error("BridgeTopic")
	}
	if got := DualFilters("meshsat/+/mo/decoded"); !reflect.DeepEqual(got, []string{"meshsat/+/mo/decoded", "meshsat/+/+/mo/decoded"}) {
		t.Errorf("DualFilters: %v", got)
	}
	if got := DualFilters("meshsat/bridge/+/birth"); !reflect.DeepEqual(got, []string{"meshsat/bridge/+/birth", "meshsat/+/bridge/+/birth"}) {
		t.Errorf("DualFilters bridge: %v", got)
	}
	// Round trip: what the builders emit, the parser reads back.
	for _, tenant := range []string{"default", "t_x"} {
		tp, dev, suf, ok := ParseDeviceTopic(TopicPositionFor(tenant, "300"))
		if !ok || tp != tenant || dev != "300" || suf != "position" {
			t.Errorf("round trip %s: %q %q %q %v", tenant, tp, dev, suf, ok)
		}
	}
}

// TestPhoneNumberDeviceIDsAreWildcardSafe: a phone number starts with "+",
// an MQTT wildcard, and a publish to meshsat/+3165.../mo/decoded is refused
// by the broker with a dropped connection (MESHSAT-1022). Builders encode
// the segment, the parser decodes it, consumers see the number unchanged.
func TestPhoneNumberDeviceIDsAreWildcardSafe(t *testing.T) {
	const phone = "+31600000001"
	for _, tc := range []struct{ name, topic string }{
		{"legacy decoded", TopicMODecoded(phone)},
		{"legacy raw", TopicMORaw(phone)},
		{"tenant decoded", TopicMODecodedFor("default", phone)},
		{"other tenant decoded", TopicMODecodedFor("t_x", phone)},
		{"position", TopicPositionFor("default", phone)},
	} {
		if err := CheckPublishTopic(tc.topic); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got := ExtractDeviceID(tc.topic); got != phone {
			t.Errorf("%s: device from %q = %q, want %q", tc.name, tc.topic, got, phone)
		}
	}
	if got, want := TopicMODecodedFor("default", phone), "meshsat/%2B31600000001/mo/decoded"; got != want {
		t.Errorf("wire form = %q, want %q", got, want)
	}
	for _, id := range []string{"300234060000002", "+31600000001", "a#b", "x/y", "50%", "%2B"} {
		if got := DecodeSegment(EncodeSegment(id)); got != id {
			t.Errorf("round trip %q -> %q", id, got)
		}
		if err := CheckPublishTopic(DeviceTopic("default", id, "sos")); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	if err := CheckPublishTopic("meshsat/+31600000001/mo/decoded"); err == nil {
		t.Error("a raw + in a publish topic must be refused")
	}
	if err := CheckPublishTopic("meshsat/#"); err == nil {
		t.Error("a # in a publish topic must be refused")
	}
	// A subscription filter still parses to the raw segment, so the dual
	// filters keep matching encoded device topics at the broker.
	if _, dev, _, ok := ParseDeviceTopic("meshsat/+/mo/decoded"); !ok || dev != "+" {
		t.Errorf("filter parse: %q %v", dev, ok)
	}
}

// The export topic for CoT (MESHSAT-1458) is the bridge's own subtree in both
// shapes, and nothing else parses as one.
func TestTAKCotOutTopics(t *testing.T) {
	if got := TopicTAKCotOutFor(DefaultTenant, "kit1"); got != "meshsat/bridge/kit1/tak/cot/out" {
		t.Errorf("default tenant: %q", got)
	}
	if got := TopicTAKCotOutFor("t_x", "kit1"); got != "meshsat/t_x/bridge/kit1/tak/cot/out" {
		t.Errorf("tenant: %q", got)
	}
	// An id is percent-encoded like every other id in a topic, and decoded back.
	enc := TopicTAKCotOutFor("t_x", "ios+1/2")
	if enc != "meshsat/t_x/bridge/ios%2B1%2F2/tak/cot/out" {
		t.Errorf("encoded: %q", enc)
	}
	if tenant, id, ok := ParseTAKCotOut(enc); !ok || tenant != "t_x" || id != "ios+1/2" {
		t.Errorf("round trip: %q %q %v", tenant, id, ok)
	}

	want := []string{"meshsat/bridge/+/tak/cot/out", "meshsat/+/bridge/+/tak/cot/out"}
	if got := TAKCotOutFilters(); !reflect.DeepEqual(got, want) {
		t.Errorf("filters = %v, want %v", got, want)
	}

	for topic, c := range map[string]struct {
		tenant, id string
		ok         bool
	}{
		"meshsat/bridge/kit1/tak/cot/out":     {DefaultTenant, "kit1", true},
		"meshsat/t_x/bridge/kit1/tak/cot/out": {"t_x", "kit1", true},
		// The shape the clients shipped with, which no bridge was ever allowed to
		// publish, and its tenant form.
		"meshsat/kit1/tak/cot/out":     {"", "", false},
		"meshsat/t_x/kit1/tak/cot/out": {"", "", false},
		// What the wide device grants (mo.>, status.>, sms.>) can reach in another
		// tenant's namespace. Not a bridge topic, so not an export.
		"meshsat/t_x/mo/tak/cot/out":     {"", "", false},
		"meshsat/t_x/status/tak/cot/out": {"", "", false},
		"meshsat/t_x/sms/tak/cot/out":    {"", "", false},
		// The same grants DO reach a bridge-shaped topic with a made-up id. It
		// parses; what refuses it is that no such bridge is registered.
		"meshsat/bridge/mo/tak/cot/out": {DefaultTenant, "mo", true},
		// Not exactly tak/cot/out under the bridge.
		"meshsat/bridge/kit1/tak/cot/in":        {"", "", false},
		"meshsat/bridge/kit1/tak/cot/out/extra": {"", "", false},
		"meshsat/bridge/kit1/x/tak/cot/out":     {"", "", false},
		"meshsat/bridge/kit1/tak/cot":           {"", "", false},
		"meshsat/bridge//tak/cot/out":           {"", "", false},
		// A reserved word is never a tenant.
		"meshsat/hub/bridge/kit1/tak/cot/out":       {"", "", false},
		"meshsat/broadcast/bridge/kit1/tak/cot/out": {"", "", false},
		"other/bridge/kit1/tak/cot/out":             {"", "", false},
	} {
		tenant, id, ok := ParseTAKCotOut(topic)
		if ok != c.ok || tenant != c.tenant || id != c.id {
			t.Errorf("ParseTAKCotOut(%q) = %q %q %v, want %q %q %v", topic, tenant, id, ok, c.tenant, c.id, c.ok)
		}
	}
}
