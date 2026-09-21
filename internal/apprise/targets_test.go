package apprise

import (
	"context"
	"errors"
	"testing"
)

// MESHSAT-1294: only a scheme:// target is Apprise's; a list without one is not
// an Apprise failure, and a list with one and no server still is.
func TestOnlyURLsAreAppriseTargets(t *testing.T) {
	for target, want := range map[string]bool{
		"tgram://bot/chat": true, "mailto://u:p@example.org": true, "json://u:p@host/path": true,
		"+31600000001": false, "ops@example.org": false, "field-alerts": false, "://nope": false, "a b://x": false,
	} {
		if IsURL(target) != want {
			t.Errorf("IsURL(%q) = %v", target, !want)
		}
	}
	n := NewNotifierPool(NewClientPool(nil, nil))
	if err := n.Notify(context.Background(), []string{"+31600000001", "ops@example.org"}, "s", "b"); err != nil {
		t.Fatalf("a page with no Apprise URL reported an Apprise failure: %v", err)
	}
	if err := n.Notify(context.Background(), []string{"tgram://bot/chat"}, "s", "b"); !errors.Is(err, errNoAppriseAccount) {
		t.Fatalf("an Apprise URL with no server must still fail loudly: %v", err)
	}
}
