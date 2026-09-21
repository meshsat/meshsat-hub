package email

import (
	"context"
	"errors"
	"testing"
)

// MESHSAT-1294: a chain of phone numbers on a tenant without mail is not a mail
// failure; an address with no gateway still is; a URL carrying credentials is
// never an address.
func TestOnlyAddressesAreEmails(t *testing.T) {
	n := NewNotifier(nil)
	if err := n.Notify(context.Background(), []string{"+31600000001", "field-alerts", "json://u:p@host.example"}, "s", "b"); err != nil {
		t.Fatalf("a page with no address reported a mail failure: %v", err)
	}
	if err := n.Notify(context.Background(), []string{"ops@example.org"}, "s", "b"); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("an address with no gateway must still fail loudly: %v", err)
	}
	if isEmailAddress("json://u:p@host.example") || isEmailAddress("tgram://a@b.c") {
		t.Fatal("a URL was taken for an email address")
	}
}
