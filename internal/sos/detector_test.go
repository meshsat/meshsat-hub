package sos

import (
	"testing"
)

func TestDetectSOS(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	tests := []struct {
		name       string
		msg        moDecodedMsg
		wantKW     string
		wantSource string
	}{
		{
			name:       "SOS keyword",
			msg:        moDecodedMsg{IMEI: "123", Text: "SOS need help at camp"},
			wantKW:     "SOS",
			wantSource: "keyword",
		},
		{
			name:       "SOS keyword lowercase",
			msg:        moDecodedMsg{IMEI: "123", Text: "sos trapped under debris"},
			wantKW:     "SOS",
			wantSource: "keyword",
		},
		{
			name:       "MAYDAY keyword",
			msg:        moDecodedMsg{IMEI: "123", Text: "mayday mayday vessel sinking"},
			wantKW:     "MAYDAY",
			wantSource: "keyword",
		},
		{
			name:       "EMERGENCY keyword",
			msg:        moDecodedMsg{IMEI: "123", Text: "Emergency at base camp"},
			wantKW:     "EMERGENCY",
			wantSource: "keyword",
		},
		{
			name:       "explicit sos field true",
			msg:        moDecodedMsg{IMEI: "123", Text: "button pressed", SOS: boolPtr(true)},
			wantKW:     "",
			wantSource: "field",
		},
		{
			name:       "explicit sos field false",
			msg:        moDecodedMsg{IMEI: "123", Text: "normal message", SOS: boolPtr(false)},
			wantKW:     "",
			wantSource: "",
		},
		{
			name:       "no SOS indicators",
			msg:        moDecodedMsg{IMEI: "123", Text: "All clear, moving to checkpoint B"},
			wantKW:     "",
			wantSource: "",
		},
		{
			name:       "empty text no SOS",
			msg:        moDecodedMsg{IMEI: "123", Text: ""},
			wantKW:     "",
			wantSource: "",
		},
		{
			name:       "SOS embedded in word",
			msg:        moDecodedMsg{IMEI: "123", Text: "SOSTENUTO is a music term"},
			wantKW:     "SOS",
			wantSource: "keyword",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kw, source := detectSOS(tt.msg)
			if kw != tt.wantKW {
				t.Errorf("keyword = %q, want %q", kw, tt.wantKW)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}
