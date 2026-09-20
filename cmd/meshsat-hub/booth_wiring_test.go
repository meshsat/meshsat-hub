package main

import (
	"strings"
	"testing"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// The booth return leg subscribes with a WILDCARD, and the wildcard must survive
// into the filter.
//
// This is the regression for the first booth round trip, where the forward leg
// worked, the kit replied, the Hub received the reply on its routing path, and
// the booth subscriber never saw one message. The filter had been built with
// TopicMODecoded("+"), and that builder runs the device id through
// EncodeSegment, which percent-encodes "+" to "%2B" so that a phone number can
// never be read as a wildcard (Critical Rule 18). The subscription was therefore
// for a device literally named "+", and it matched nothing, silently.
func TestMeshReplyFiltersAreRealWildcards(t *testing.T) {
	filters := hubmqtt.DualFilters("meshsat/+/mo/decoded")
	if len(filters) == 0 {
		t.Fatal("no filters produced")
	}
	for _, f := range filters {
		if strings.Contains(f, "%2B") {
			t.Errorf("filter %q carries a percent-encoded wildcard; it will match nothing", f)
		}
		if !strings.Contains(f, "+") {
			t.Errorf("filter %q has no wildcard at all", f)
		}
	}

	// And it must actually match the topic a kit publishes on. The Bridge's
	// event tap publishes to meshsat/{device_id}/mo/decoded (MESHSAT-1178).
	const real = "meshsat/!0a0b0c0d/mo/decoded"
	matched := false
	for _, f := range filters {
		if mqttFilterMatches(f, real) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("no filter in %v matches %q, so a kit's mesh reply is never delivered", filters, real)
	}
}

// The encoder is doing its job and must keep doing it: a real device id that
// contains "+" -- an E.164 number on the SMS paths -- still has to be escaped.
func TestDeviceIDWildcardIsStillEscapedForRealIDs(t *testing.T) {
	got := hubmqtt.TopicMODecoded("+31600000001")
	if strings.Contains(got, "/+31") {
		t.Errorf("a phone number reached the topic unescaped: %q", got)
	}
	if !strings.Contains(got, "%2B") {
		t.Errorf("expected the leading + to be percent-encoded: %q", got)
	}
}

// mqttFilterMatches is MQTT topic matching, enough for these filters: "+"
// matches exactly one level, "#" matches the rest.
func mqttFilterMatches(filter, topic string) bool {
	f := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if seg != "+" && seg != tp[i] {
			return false
		}
	}
	return len(f) == len(tp)
}
