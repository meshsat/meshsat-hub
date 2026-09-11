package tenancy

import (
	"context"
	"testing"
)

func TestIsPlatformTopic(t *testing.T) {
	st := &stubStore{
		devices: map[string]string{"300234063904190": "default", "300234069999777": "t_known"},
		bridges: map[string]string{"kit-plat": "default", "kit-cust": "t_known"},
	}
	r := NewResolver(st, "default", 0)

	tests := []struct {
		name  string
		topic string
		want  bool
	}{
		{"platform device, untenanted topic", "meshsat/300234063904190/position", true},
		{"unregistered device, untenanted topic", "meshsat/300234060000001/sos", true},
		{"customer device, its tenant's topic", "meshsat/t_known/300234069999777/position", false},
		// The owner of record wins: a customer's device is not pulled into
		// the platform by an untenanted topic.
		{"customer device, untenanted topic", "meshsat/300234069999777/mo/decoded", false},
		// A topic naming a customer tenant is never platform traffic, whoever
		// the device belongs to.
		{"platform device, customer-prefixed topic", "meshsat/t_known/300234063904190/position", false},
		{"unregistered device, customer-prefixed topic", "meshsat/t_known/300234060000002/telemetry", false},
		{"platform bridge", "meshsat/bridge/kit-plat/birth", true},
		{"platform bridge, device birth", "meshsat/bridge/kit-plat/device/n1/birth", true},
		{"customer bridge, its tenant's topic", "meshsat/t_known/bridge/kit-cust/health", false},
		{"customer bridge, untenanted topic", "meshsat/bridge/kit-cust/birth", false},
		{"hub topic", "meshsat/hub/status", false},
		{"broadcast topic", "meshsat/broadcast/tak/cot/in", false},
		{"not a meshsat topic", "other/300234063904190/position", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.IsPlatformTopic(context.Background(), tt.topic); got != tt.want {
				t.Errorf("IsPlatformTopic(%q) = %v, want %v", tt.topic, got, tt.want)
			}
		})
	}
}
