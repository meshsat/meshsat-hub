package fsutil

import (
	"path/filepath"
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	if got, err := ValidateBaseURL("https://api.cloudloop.com/"); err != nil || got != "https://api.cloudloop.com" {
		t.Fatalf("got %q err %v", got, err)
	}
	for _, bad := range []string{"", "ftp://x", "api.cloudloop.com", "http://user:pw@host", "http:///path"} {
		if _, err := ValidateBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAbsOnly(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"relative/file":          "",
		"/data/x/../secrets.env": "",
		"/data/secrets.env":      "/data/secrets.env",
		"/data//nats/ca.crt":     "",
		"../etc/passwd":          "",
	}
	for in, want := range cases {
		if got := AbsOnly(in); got != want {
			t.Errorf("AbsOnly(%q) = %q, want %q", in, got, want)
		}
	}
	// The intended call pattern: Clean first, then AbsOnly.
	if got := AbsOnly(filepath.Clean("/data/x/../nats/ca.crt")); got != "/data/nats/ca.crt" {
		t.Errorf("Clean+AbsOnly = %q", got)
	}
}
