package sms

import (
	"context"
	"testing"
)

// MESHSAT-1294: a chain with no phone number is not an SMS failure on a tenant
// without Twilio; a phone number with no account still is.
func TestOnlyPhoneNumbersAreSMS(t *testing.T) {
	n := NewNotifier(nil)
	if err := n.Notify(context.Background(), []string{"ops@example.org", "field-alerts"}, "s", "b"); err != nil {
		t.Fatalf("a page with no phone number reported an SMS failure: %v", err)
	}
	if err := n.Notify(context.Background(), []string{"+31600000001"}, "s", "b"); err == nil {
		t.Fatal("a phone number with no Twilio account must still fail loudly")
	}
}
