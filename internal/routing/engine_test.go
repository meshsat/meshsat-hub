package routing

import (
	"testing"
)

func TestMatchSource(t *testing.T) {
	tests := []struct {
		routeSource, msgSource string
		want                   bool
	}{
		{"*", "iridium", true},
		{"*", "globalstar", true},
		{"iridium", "iridium", true},
		{"iridium", "Iridium", true},
		{"iridium", "globalstar", false},
		{"globalstar", "iridium", false},
		{"sms", "sms", true},
		{"satellite", "iridium", true},
		{"satellite", "iridium_imt", true},
		{"satellite", "globalstar", true},
		{"Satellite", "Iridium", true},
		{"satellite", "sms", false},
		{"satellite", "email", false},
		{"satellite", "", false},
	}
	for _, tt := range tests {
		if got := matchSource(tt.routeSource, tt.msgSource); got != tt.want {
			t.Errorf("matchSource(%q, %q) = %v, want %v", tt.routeSource, tt.msgSource, got, tt.want)
		}
	}
}

func TestMatchSenders(t *testing.T) {
	cases := []struct {
		senders, origin string
		want            bool
	}{
		{"", "+31600000001", true},
		{"*", "anything", true},
		{"+31600000001", "+31600000001", true},
		{"+31600000001", "+31600000002", false},
		{"+31600000001, +31600000002", "+31600000002", true},
		{" 300234065000001 ,+31600000001", "300234065000001", true},
		{"+31600000001", "", false},
		{"abc", "ABC", true},
	}
	for _, c := range cases {
		if got := matchSenders(c.senders, c.origin); got != c.want {
			t.Errorf("matchSenders(%q, %q) = %v, want %v", c.senders, c.origin, got, c.want)
		}
	}
}

func TestMatchFilter(t *testing.T) {
	tests := []struct {
		filter, deviceID, text string
		want                   bool
	}{
		{"", "dev1", "any text", true},                     // empty filter matches all
		{"dev1", "dev1", "any", true},                      // exact device match
		{"dev2", "dev1", "any", false},                     // different device
		{"SOS", "dev1", "SOS need help", true},             // keyword in text
		{"sos", "dev1", "SOS need help", true},             // case insensitive
		{"MAYDAY", "dev1", "all clear", false},             // keyword not in text
		{"dev1", "dev1", "message from dev1 device", true}, // device match takes priority
	}
	for _, tt := range tests {
		if got := matchFilter(tt.filter, tt.deviceID, tt.text); got != tt.want {
			t.Errorf("matchFilter(%q, %q, %q) = %v, want %v", tt.filter, tt.deviceID, tt.text, got, tt.want)
		}
	}
}

func TestDefaultRoutes(t *testing.T) {
	routes := DefaultRoutes()
	if len(routes) != 5 {
		t.Fatalf("expected 5 default routes, got %d", len(routes))
	}
	for _, r := range routes {
		if r.SourceType != "satellite" {
			t.Errorf("default route %q: source = %q, want satellite", r.Name, r.SourceType)
		}
		if matchSource(r.SourceType, "sms") {
			t.Errorf("default route %q must not fire on an SMS", r.Name)
		}
		if !matchSource(r.SourceType, "iridium") {
			t.Errorf("default route %q must fire on an Iridium message", r.Name)
		}
		if !r.Enabled {
			t.Errorf("default route %q: expected enabled", r.Name)
		}
	}

	// Check all expected destination types present.
	destTypes := map[string]bool{}
	for _, r := range routes {
		destTypes[r.DestinationType] = true
	}
	for _, dt := range []string{"tak", "aprs", "webhook", "notification", "mqtt"} {
		if !destTypes[dt] {
			t.Errorf("missing default route for destination %q", dt)
		}
	}
}

// A text that belongs to a lane is that lane's to deliver: no route is even
// looked up for it. The engine here has no store and no tenant resolver, so
// anything that got past the lane filter would dereference nil; the claimed
// text returning quietly is the proof it never got that far, and the ordinary
// text panicking is the proof the filter is what made the difference.
func TestALaneTextIsNotRouted(t *testing.T) {
	e := &Engine{handlers: map[string]DestinationHandler{}, cachedRoutes: map[string]routeCache{}}
	e.SetLaneFilter(func(text string) bool { return len(text) > 0 && text[0] == '*' })

	const topic = "meshsat/%2B31600000001/mo/decoded"
	e.handleMODecoded(topic, []byte(`{"id":"m1","channel":"sms","text":"*F8 on my way"}`))

	reached := func() (got bool) {
		defer func() { got = recover() != nil }()
		e.handleMODecoded(topic, []byte(`{"id":"m2","channel":"sms","text":"plain kit to kit text"}`))
		return false
	}()
	if !reached {
		t.Fatal("an ordinary text did not reach route evaluation; this test no longer proves anything")
	}
}

// A route source may pin the IMT topic ("iridium_imt:IMT_TOPIC_RAW"): the
// kit-to-kit Reticulum relay between two 9704s must never fire on an SBD or
// SMS message, and a bare "iridium_imt" means any IMT message. [MESHSAT-1352]
func TestMatchSourceTopic(t *testing.T) {
	tests := []struct {
		routeSource, msgSource, topic string
		want                          bool
	}{
		{"iridium_imt:IMT_TOPIC_RAW", "iridium", "IMT_TOPIC_RAW", true},
		{"iridium_imt:imt_topic_raw", "iridium", "IMT_TOPIC_RAW", true},
		{"iridium_imt:IMT_TOPIC_RAW", "iridium", "IMT_TOPIC_PURPLE", false},
		{"iridium_imt:IMT_TOPIC_RAW", "iridium", "", false}, // SBD: no topic
		{"iridium_imt:IMT_TOPIC_RAW", "sms", "", false},
		{"iridium_imt", "iridium", "IMT_TOPIC_PURPLE", true},
		{"iridium_imt", "iridium", "", false},
		{"satellite:IMT_TOPIC_RAW", "iridium", "IMT_TOPIC_RAW", true},
		{"satellite:IMT_TOPIC_RAW", "iridium", "", false},
		{"satellite", "iridium", "", true},
		{"*", "sms", "", true},
		{"*:IMT_TOPIC_RAW", "iridium", "IMT_TOPIC_RAW", true},
		{"*:IMT_TOPIC_RAW", "sms", "", false},
		{"iridium", "iridium", "IMT_TOPIC_RAW", true},
	}
	for _, tt := range tests {
		if got := matchSourceTopic(tt.routeSource, tt.msgSource, tt.topic); got != tt.want {
			t.Errorf("matchSourceTopic(%q, %q, %q) = %v, want %v", tt.routeSource, tt.msgSource, tt.topic, got, tt.want)
		}
	}
}
