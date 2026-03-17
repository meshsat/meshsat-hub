package tak

import "testing"

func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"300234063904190", "4190"},
		{"abcd", "abcd"},
		{"ab", "ab"},
		{"", ""},
		{"12345678", "5678"},
	}
	for _, tt := range tests {
		got := shortID(tt.input)
		if got != tt.expected {
			t.Errorf("shortID(%q): got %q, want %q", tt.input, got, tt.expected)
		}
	}
}
