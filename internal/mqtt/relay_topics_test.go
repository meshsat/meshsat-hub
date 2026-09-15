package mqtt

import "testing"

func TestRelayTopicsRoundTripInBothShapes(t *testing.T) {
	cases := []struct{ tenant, bridge, client, dir, want string }{
		{DefaultTenant, "kit-a", "phone-1", "up", "meshsat/relay/kit-a/phone-1/up"},
		{"acme", "kit-a", "phone-1", "down", "meshsat/acme/relay/kit-a/phone-1/down"},
		// ids with reserved characters are encoded on the wire and decoded back
		{"acme", "kit+1", "+316", "up", "meshsat/acme/relay/kit%2B1/%2B316/up"},
	}
	for _, c := range cases {
		got := RelayTopic(c.tenant, c.bridge, c.client, c.dir)
		if got != c.want {
			t.Errorf("RelayTopic = %q, want %q", got, c.want)
		}
		if err := CheckPublishTopic(got); err != nil {
			t.Errorf("%q: %v", got, err)
		}
		tenant, bridge, client, dir, ok := ParseRelayTopic(got)
		if !ok || tenant != c.tenant || bridge != c.bridge || client != c.client || dir != c.dir {
			t.Errorf("ParseRelayTopic(%q) = %q %q %q %q %v", got, tenant, bridge, client, dir, ok)
		}
	}
}

func TestRelayTopicsAreNotDeviceOrBridgeTopics(t *testing.T) {
	for _, topic := range []string{"meshsat/relay/kit-a/phone-1/up", "meshsat/acme/relay/kit-a/phone-1/down"} {
		if _, _, _, ok := ParseDeviceTopic(topic); ok {
			t.Errorf("%q parsed as a device topic", topic)
		}
		if _, _, _, ok := ParseBridgeTopic(topic); ok {
			t.Errorf("%q parsed as a bridge topic", topic)
		}
	}
	for _, bad := range []string{"meshsat/relay/kit-a/phone-1/sideways", "meshsat/relay/kit-a/up", "meshsat/hub/relay/a/b/up", "meshsat//relay/a/b/up", "meshsat/relay//b/up"} {
		if _, _, _, _, ok := ParseRelayTopic(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRelayFiltersCoverBothShapes(t *testing.T) {
	up := RelayFilters("up")
	if len(up) != 2 || up[0] != "meshsat/relay/+/+/up" || up[1] != "meshsat/+/relay/+/+/up" {
		t.Fatalf("filters %v", up)
	}
}
