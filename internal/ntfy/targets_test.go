package ntfy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// MESHSAT-1294. The escalation engine hands every backend every target. ntfy
// published to all of them, so a page to "+31..." on a tenant with ntfy went to
// a public ntfy topic named after the phone number; and on a tenant without
// ntfy, a chain of phone numbers was logged as a failed page.
func TestOnlyTopicsAreNtfys(t *testing.T) {
	for target, want := range map[string]bool{
		"field-alerts": true, "sos_team": true,
		"+31600000001": false, "ops@example.org": false, "tgram://bot/chat": false, "": false,
		"has space": false, strings.Repeat("a", 65): false,
	} {
		if IsTopic(target) != want {
			t.Errorf("IsTopic(%q) = %v", target, !want)
		}
	}
}

func TestAPhoneOnlyPageIsNotAnNtfyFailure(t *testing.T) {
	n := NewNotifierPool(NewClientPool(nil, nil)) // no ntfy server anywhere
	if err := n.Notify(context.Background(), []string{"+31600000001", "ops@example.org"}, "s", "b"); err != nil {
		t.Fatalf("a page with no ntfy target reported an ntfy failure: %v", err)
	}
	if err := n.Notify(context.Background(), []string{"field-alerts"}, "s", "b"); !errors.Is(err, errNoNtfyAccount) {
		t.Fatalf("an ntfy target with no server must still fail loudly: %v", err)
	}
}

func TestAPhoneNumberIsNeverPublishedAsATopic(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := NewNotifierPool(NewClientPool(New(srv.URL), nil))
	if err := n.Notify(context.Background(), []string{"+31600000001", "ops@example.org", "json://u:p@host", "field-alerts"}, "s", "b"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/field-alerts" {
		t.Fatalf("published to %v, want only /field-alerts", paths)
	}
}
