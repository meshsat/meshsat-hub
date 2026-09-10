package vat

import "testing"

// The rule this package exists for. Before it, every buyer was invoiced as
// Dutch at 21% because the country was never asked for -- correct for the
// Netherlands, defensible inside the EU under the threshold, and simply wrong
// for anyone outside it.
func TestTheTreatmentFollowsTheBuyersCountry(t *testing.T) {
	for _, tc := range []struct {
		country string
		charge  bool
		why     string
	}{
		{"NL", true, "the seller's own country"},
		{"nl", true, "case is not the customer's problem"},
		{" NL ", true, "nor is whitespace"},
		{"DE", true, "EU consumer, under the threshold"},
		{"IE", true, "EU consumer, under the threshold"},
		{"GR", true, "Greece"},
		{"EL", true, "Greece again, as payment data spells it"},
		{"US", false, "outside the EU: no EU VAT applies at all"},
		{"GB", false, "outside the EU since Brexit"},
		{"CH", false, "never was in it"},
		{"NO", false, "EEA is not the EU VAT area"},
		{"", false, "unknown: guessing is how somebody gets the wrong document"},
		{"   ", false, "blank is unknown"},
		{"ZZ", false, "not a country we know"},
	} {
		got := For(tc.country)
		if got.Charge != tc.charge {
			t.Errorf("For(%q).Charge = %v, want %v (%s)", tc.country, got.Charge, tc.charge, tc.why)
		}
		if got.Charge && got.Basis == "" {
			t.Errorf("For(%q) charges VAT but records no basis", tc.country)
		}
		if !got.Charge && got.Reason == "" {
			t.Errorf("For(%q) parks but gives the operator no reason", tc.country)
		}
	}
}

// A parked receipt is read by a person deciding what to do, so the reason has
// to name the problem and the fix rather than say "invalid".
func TestAParkedReasonIsActionable(t *testing.T) {
	us := For("US")
	for _, want := range []string{"outside the EU", "US"} {
		if !contains(us.Reason, want) {
			t.Errorf("non-EU reason %q does not mention %q", us.Reason, want)
		}
	}
	unknown := For("")
	for _, want := range []string{"no country on file", "requeue"} {
		if !contains(unknown.Reason, want) {
			t.Errorf("unknown-country reason %q does not mention %q", unknown.Reason, want)
		}
	}
}

// The threshold counts supplies to OTHER member states. Counting our own
// domestic sales would raise a false alarm on the busiest possible month and
// push the business toward an OSS registration it does not need.
func TestTheThresholdExcludesDomesticSales(t *testing.T) {
	if CountsTowardThreshold("NL") {
		t.Error("a Dutch sale counts toward the cross-border threshold; it must not")
	}
	for _, c := range []string{"DE", "FR", "IE", "ES"} {
		if !CountsTowardThreshold(c) {
			t.Errorf("a sale to %s does not count toward the threshold; it must", c)
		}
	}
	for _, c := range []string{"US", "GB", "", "ZZ"} {
		if CountsTowardThreshold(c) {
			t.Errorf("a sale to %q counts toward the EU threshold; it is not an EU sale", c)
		}
	}
	if Threshold != 1_000_000 {
		t.Errorf("threshold = %d cents, want 10 000 euro", Threshold)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
