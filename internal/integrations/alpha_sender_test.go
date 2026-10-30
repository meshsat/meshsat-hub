package integrations

import (
	"strings"
	"testing"
)

// MESHSAT-1217. A sender name Twilio would refuse, or one with no country to
// use it for, is a 400 on the form rather than an alert page that fails or a
// setting that silently does nothing.
func TestTwilioSenderNameIsCheckedOnSave(t *testing.T) {
	svc, ctx := newSvc(t)
	base := func(alpha, countries string) map[string]string {
		m := map[string]string{"account_sid": "AC1", "auth_token": "tok", "from_number": "+3197010000000"}
		if alpha != "" {
			m["alpha_sender"] = alpha
		}
		if countries != "" {
			m["alpha_sender_countries"] = countries
		}
		return m
	}
	for _, bad := range []struct{ alpha, countries, field string }{
		{"MeshSat!", "+31", "Sender name"},
		{"MeshSatAlerts", "+31", "Sender name"},
		{"2026", "+31", "Sender name"},
		{"MeshSat", "", "calling codes"},
		{"MeshSat", "NL", "Countries"},
		{"MeshSat", "+31, 32", "Countries"},
	} {
		_, err := svc.Set(ctx, "t-alpha", ProviderTwilio, base(bad.alpha, bad.countries))
		if err == nil {
			t.Errorf("sender %q for %q was accepted", bad.alpha, bad.countries)
			continue
		}
		if !strings.Contains(err.Error(), bad.field) {
			t.Errorf("sender %q for %q: error %q does not name %q", bad.alpha, bad.countries, err, bad.field)
		}
	}
	acct, err := svc.Set(ctx, "t-alpha", ProviderTwilio, base("MeshSat", "+31, +32"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Fields["alpha_sender"] != "MeshSat" || acct.Fields["alpha_sender_countries"] != "+31, +32" {
		t.Errorf("stored %v", acct.Fields)
	}
	// Countries alone, with no name, are harmless and kept.
	if _, err := svc.Set(ctx, "t-alpha2", ProviderTwilio, base("", "+31")); err != nil {
		t.Errorf("countries without a name refused: %v", err)
	}
}
