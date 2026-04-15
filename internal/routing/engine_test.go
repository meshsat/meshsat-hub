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
	}
	for _, tt := range tests {
		if got := matchSource(tt.routeSource, tt.msgSource); got != tt.want {
			t.Errorf("matchSource(%q, %q) = %v, want %v", tt.routeSource, tt.msgSource, got, tt.want)
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
		if r.SourceType != "*" {
			t.Errorf("default route %q: source = %q, want *", r.Name, r.SourceType)
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
