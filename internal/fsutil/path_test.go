package fsutil

import "testing"

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
