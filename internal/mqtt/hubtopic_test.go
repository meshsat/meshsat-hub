package mqtt

import "testing"

func TestIsHubTopic(t *testing.T) {
	cases := map[string]bool{
		"meshsat/hub/sms/inbound":                true,
		"meshsat/hub/oob/reply":                  true,
		"meshsat/broadcast/tak/cot/in":           true,
		"meshsat/bridge/kit-a/hemb":              true,
		"meshsat/relay/kit-a/phone-1/up":         true,
		"meshsat/android-001/sms/inbound":        false,
		"meshsat/t-acme/android-001/sms/inbound": false,
		"meshsat/%2B31600000002/mo/decoded":      false,
		"meshsat":                                false,
		"other/hub/sms/inbound":                  false,
	}
	for topic, want := range cases {
		if got := IsHubTopic(topic); got != want {
			t.Errorf("IsHubTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}
