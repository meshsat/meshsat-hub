package routing

import (
	"testing"
)

func TestParseRecipients(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"+31612345678", 1},
		{"+31612345678,+14155551234", 2},
		{"+31612345678, +14155551234, +442012345678", 3},
		{"user@example.com", 1},
		{"a@b.com, c@d.com", 2},
		{" , , ", 0},
	}
	for _, tt := range tests {
		got := parseRecipients(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseRecipients(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestFormatRoutedSMS(t *testing.T) {
	// Short message.
	msg := formatRoutedSMS("dev1", "SOS help")
	if msg != "[dev1] SOS help" {
		t.Errorf("got %q", msg)
	}

	// Long message truncated.
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	msg = formatRoutedSMS("dev1", long)
	if len(msg) != 160 {
		t.Errorf("len = %d, want 160", len(msg))
	}
	if msg[157:] != "..." {
		t.Errorf("expected trailing ..., got %q", msg[157:])
	}
}
