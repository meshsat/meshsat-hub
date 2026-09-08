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
