package mqtt

import "testing"

// The Hub's own copy of a satellite MO (MESHSAT-1685) round-trips for both
// tenant shapes, and nothing else parses as one.
func TestSatMOTopic(t *testing.T) {
	for _, tc := range []struct{ tenant, imei, topic string }{
		{DefaultTenant, "300434060000101", "meshsat/hub/satmo/300434060000101"},
		{"t_alpha", "300434060000101", "meshsat/t_alpha/hub/satmo/300434060000101"},
	} {
		if got := TopicSatMOFor(tc.tenant, tc.imei); got != tc.topic {
			t.Fatalf("TopicSatMOFor = %q, want %q", got, tc.topic)
		}
		tenant, imei, ok := ParseSatMOTopic(tc.topic)
		if !ok || tenant != tc.tenant || imei != tc.imei {
			t.Fatalf("ParseSatMOTopic(%q) = %q %q %v", tc.topic, tenant, imei, ok)
		}
	}
	for _, bad := range []string{
		"meshsat/300434060000101/mo/decoded", "meshsat/t_alpha/300434060000101/mo/decoded",
		"meshsat/hub/satmo/", "meshsat/hub/credits", "meshsat/t_alpha/hub/satmo",
	} {
		if _, _, ok := ParseSatMOTopic(bad); ok {
			t.Errorf("%q parsed as a satmo topic", bad)
		}
	}
}
