package mqtt

import "testing"

func TestTopicFunctions(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) string
		arg  string
		want string
	}{
		{"MORaw", TopicMORaw, "300234065123456", "meshsat/300234065123456/mo/raw"},
		{"MODecoded", TopicMODecoded, "300234065123456", "meshsat/300234065123456/mo/decoded"},
		{"MTSend", TopicMTSend, "300234065123456", "meshsat/300234065123456/mt/send"},
		{"MTStatus", TopicMTStatus, "300234065123456", "meshsat/300234065123456/mt/status"},
		{"Position", TopicPosition, "dev1", "meshsat/dev1/position"},
		{"SOS", TopicSOS, "dev1", "meshsat/dev1/sos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn(tt.arg)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractDeviceID(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"meshsat/300234065123456/mt/send", "300234065123456"},
		{"meshsat/dev1/mt/send", "dev1"},
		{"meshsat/abc/mo/raw", "abc"},
		{"invalid", ""},
		{"meshsat/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := ExtractDeviceID(tt.topic)
			if got != tt.want {
				t.Errorf("ExtractDeviceID(%q) = %q, want %q", tt.topic, got, tt.want)
			}
		})
	}
}
