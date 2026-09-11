package mail

import (
	"strings"
	"testing"
)

// The first real donation arrived under the billing system's subscription
// template. These are the three sentences the giver actually received, each of
// which is false about a gift:
//
//	"...and shows the VAT included in the price."
//	"Your MeshSat Hub subscription is active for the period this payment covers."
//	"Your current plan and device usage are on the Settings page in the Hub."
//
// The donation carried no VAT at all, bought no tier, and was anonymous -- so
// there was no plan, no Settings page and no account. This message replaces it;
// the test names the wrong words so nobody reintroduces them.
func TestTheDonationReceiptDoesNotClaimASubscription(t *testing.T) {
	m := DonationReceipt("K Papadopoulos", "EUR 1,00", "MSH2026-0001")
	body := m.Text + "\n" + m.HTML

	for _, wrong := range []string{
		"VAT included in the price",
		"subscription is active",
		"device usage",
		"Settings page",
	} {
		if strings.Contains(body, wrong) {
			t.Errorf("the donation receipt still says %q, which is untrue of a gift", wrong)
		}
	}
}

// The three facts a giver needs, in both renderings. The no-VAT sentence is the
// load-bearing one: without it a document showing no tax looks like a mistake.
func TestTheDonationReceiptSaysWhatAGiftIs(t *testing.T) {
	m := DonationReceipt("K Papadopoulos", "EUR 1,00", "MSH2026-0001")

	for _, part := range []struct{ name, s string }{{"text", m.Text}, {"html", m.HTML}} {
		lower := strings.ToLower(part.s)
		for _, want := range []string{
			"no vat", // why the document shows none
			"outside the scope of btw",
			"grants no plan",
			"needs no account",
		} {
			if !strings.Contains(lower, want) {
				t.Errorf("%s rendering does not say %q", part.name, want)
			}
		}
		// The amount and the document it is attached to.
		if !strings.Contains(part.s, "EUR 1,00") {
			t.Errorf("%s rendering does not name the amount", part.name)
		}
		if !strings.Contains(part.s, "MSH2026-0001") {
			t.Errorf("%s rendering does not name the receipt", part.name)
		}
	}

	if !strings.Contains(m.Subject, "MSH2026-0001") {
		t.Errorf("subject %q does not name the receipt", m.Subject)
	}
	if !strings.Contains(strings.ToLower(m.Subject), "donation") {
		t.Errorf("subject %q does not say what it is about", m.Subject)
	}
}

// Anyone can donate without an account, so a sign-in destination is noise for
// most readers -- and a link into an application they cannot enter is the thing
// that made the return page wrong too.
func TestTheDonationReceiptDoesNotSendAGiverToASignIn(t *testing.T) {
	m := DonationReceipt("Alice", "EUR 5,00", "MSH2026-0002")
	if strings.Contains(m.Text+m.HTML, "hub.meshsat.net") {
		t.Error("the donation receipt links a giver into an app they may have no account for")
	}
}

// A document number is assigned by the billing system and could in principle be
// absent. The message still has to make sense.
func TestTheDonationReceiptSurvivesAMissingNumber(t *testing.T) {
	m := DonationReceipt("", "EUR 25,00", "")
	if strings.Contains(m.Subject, "  ") || strings.HasSuffix(m.Subject, " ") {
		t.Errorf("subject %q has a gap where the number would be", m.Subject)
	}
	if !strings.Contains(m.Text, "Your receipt is attached") {
		t.Errorf("without a number the text reads wrong:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "Hello,") {
		t.Error("an anonymous giver should get a plain greeting, not an empty name")
	}
}
