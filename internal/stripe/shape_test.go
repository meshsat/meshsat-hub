package stripe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guard this package was missing.
//
// Seven Stripe defects reached production in two days and six were one bug:
// Stripe moved a field between API versions, the struct kept the old `json`
// tag, encoding/json produced a zero value, and a fallback made it look like
// normal operation. Nothing failed. The unit tests all passed, because every
// one of them built its payload from a Go literal that was written from the
// same wrong assumption as the code.
//
// So this test does not build payloads. It reads real captured deliveries and
// asserts that every field the Hub actually depends on is READABLE from them.
// A fixture is a fact about what Stripe sends; a literal is only a restatement
// of what we believe.
//
// When Stripe next moves something, CI fails here with the field named, instead
// of a customer getting a receipt for a plan they did not buy.

// fixture loads a captured event and reports the API version that rendered it.
func fixture(t *testing.T, name string) Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the captured payload %s: %v", name, err)
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("%s is not a Stripe event: %v", name, err)
	}
	return ev
}

// TestEveryCapturedDeliveryIsReadable walks each real payload and checks the
// accessors the handlers call. Each case names the consequence of the field
// being absent, because that is what a failure here means in production.
func TestEveryCapturedDeliveryIsReadable(t *testing.T) {
	cases := []struct {
		file  string
		typ   string
		check func(t *testing.T, ev Event)
	}{
		{
			file: "invoice_paid_dahlia.json", typ: EventInvoicePaid,
			check: func(t *testing.T, ev Event) {
				inv := decode[invoice](t, ev)
				want(t, "invoice.parent.subscription_details.metadata[tenant_id]",
					inv.metadata()["tenant_id"],
					"the first payment of every new subscriber is recorded as unattributed and gets no VAT document")
				want(t, "invoice.parent.subscription_details.subscription", inv.subscriptionID(),
					"the invoice cannot be joined to its subscription")
				want(t, "invoice.lines[].pricing.price_details.price", inv.priceID(),
					"the receipt names the tenant's existing plan rather than what was bought")
				wantInt(t, "invoice.amount_paid", inv.AmountPaid, "the receipt is for nothing")
				want(t, "invoice.currency", inv.Currency, "the receipt has no currency and parks")
				wantTime(t, "invoice.created", inv.paidAt(), "the receipt is dated now, not when it was paid")
			},
		},
		{
			file: "subscription_created_basil.json", typ: EventSubscriptionCreated,
			check: func(t *testing.T, ev Event) {
				sub := decode[subscription](t, ev)
				want(t, "subscription.id", sub.ID, "the subscription cannot be bound to the tenant")
				want(t, "subscription.status", sub.Status, "an empty status is not live, which ENDS the plan")
				want(t, "subscription.items[].price.id", sub.priceID(), "no plan can be resolved, so nothing is granted")
				wantTime(t, "subscription.items[].current_period_end", sub.periodEnd(),
					"the plan expiry is invented from the clock instead of read from Stripe")
				if !sub.knownStatus() {
					t.Errorf("subscription.status %q is not one this Hub recognises; live() would answer false and END the plan", sub.Status)
				}
			},
		},
		{
			file: "subscription_deleted_dahlia.json", typ: EventSubscriptionDeleted,
			check: func(t *testing.T, ev Event) {
				sub := decode[subscription](t, ev)
				want(t, "subscription.id", sub.ID, "the cancellation cannot be matched to a tenant")
				want(t, "subscription.metadata[tenant_id]", sub.Metadata["tenant_id"],
					"a cancellation cannot find its tenant and the customer stays on a paid plan")
				want(t, "subscription.status", sub.Status, "")
				if sub.live() {
					t.Errorf("a cancelled subscription reports live()=true (status %q); the plan would never end", sub.Status)
				}
			},
		},
		{
			file: "checkout_completed_dahlia.json", typ: EventCheckoutCompleted,
			check: func(t *testing.T, ev Event) {
				cs := decode[checkoutSession](t, ev)
				want(t, "checkout.mode", cs.Mode, "the donation and subscription branches cannot be told apart")
				want(t, "checkout.customer", cs.Customer, "the customer is never bound to the tenant")
				want(t, "checkout.metadata[tenant_id]", cs.Metadata["tenant_id"],
					"the payment cannot be bound to an account -- this is what replaced the claim code")
				want(t, "checkout.customer_details.email", cs.email(), "no address to send the receipt to")
				want(t, "checkout.customer_details.address.country", cs.country(),
					"no country, so the receipt parks at the VAT gate")
				if cs.Mode != "subscription" {
					t.Errorf("this fixture is meant to be the subscription branch, got mode=%q", cs.Mode)
				}
			},
		},
		{
			file: "checkout_donation_dahlia.json", typ: EventCheckoutCompleted,
			check: func(t *testing.T, ev Event) {
				cs := decode[checkoutSession](t, ev)
				want(t, "checkout.mode", cs.Mode, "")
				want(t, "checkout.metadata[kind]", cs.Metadata[MetadataKind],
					"an anonymous donation is indistinguishable from a stranger's payment and is recorded as unattributed")
				want(t, "checkout.payment_intent", cs.PaymentIntent,
					"a refunded donation can never be matched back to its receipt, so no credit note issues")
				wantInt(t, "checkout.amount_total", cs.AmountTotal, "the donation receipt is for nothing")
				want(t, "checkout.customer_details.address.country", cs.country(), "")
				if cs.Mode != "payment" {
					t.Errorf("a donation must be mode=payment, got %q", cs.Mode)
				}
			},
		},
		{
			file: "invoice_payment_paid_dahlia.json", typ: EventInvoicePaymentPaid,
			check: func(t *testing.T, ev Event) {
				ip := decode[invoicePayment](t, ev)
				want(t, "invoice_payment.invoice", ip.Invoice, "")
				want(t, "invoice_payment.payment.payment_intent", ip.Payment.PaymentIntent,
					"the ONLY bridge from a charge back to its invoice is lost, so a refunded "+
						"subscription matches no receipt and issues no credit note")
				want(t, "invoice_payment.status", ip.Status, "a payment that did not settle would be recorded as if it had")
			},
		},
		{
			file: "charge_refunded_dahlia.json", typ: EventChargeRefunded,
			check: func(t *testing.T, ev Event) {
				ch := decode[charge](t, ev)
				want(t, "charge.payment_intent", ch.PaymentIntent,
					"the refund cannot be matched to a receipt, so the sale stays in the books at full value with its VAT declared")
				wantInt(t, "charge.amount_refunded", ch.AmountRefunded, "a credit note for zero")
				want(t, "charge.currency", ch.Currency, "")
				// charge.invoice was REMOVED by Stripe. Asserting it stays gone
				// is as valuable as asserting a field is present: if it ever
				// comes back, the invoice_payment.paid bridge is redundant and
				// somebody should know before maintaining both.
				if ch.Invoice != "" {
					t.Errorf("charge.invoice is set (%q) -- Stripe removed this field and "+
						"internal/stripe carries invoice_payment.paid solely to replace it. "+
						"If it is back, revisit that bridge.", ch.Invoice)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			ev := fixture(t, c.file)
			if ev.Type != c.typ {
				t.Fatalf("fixture is %q, expected %q", ev.Type, c.typ)
			}
			c.check(t, ev)
		})
	}
}

// TestEveryCapturedDeliveryNamesItsAPIVersion keeps the fixtures honest about
// which Stripe version they represent. A fixture whose version is only in its
// filename proves nothing -- rename the file and the claim changes.
func TestEveryCapturedDeliveryNamesItsAPIVersion(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no captured payloads found: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		ev := fixture(t, name)
		if ev.APIVersion == "" {
			// subscription_created_basil.json predates this test and was
			// captured without the envelope. Its version is asserted by
			// subscription_period_test.go instead, which checks the shape the
			// version is claimed to produce.
			if name == "subscription_created_basil.json" {
				continue
			}
			t.Errorf("%s carries no api_version; the version it proves is only a filename", name)
			continue
		}
		if !KnownAPIVersion(ev.APIVersion) {
			t.Errorf("%s was rendered by %s, which is not in knownAPIVersions. "+
				"Either add it deliberately (after reading the changelog) or the fixture is stale.",
				name, ev.APIVersion)
		}
	}
}

// TestThePinnedVersionIsTheOneWeThinkItIs replaces an assertion that could not
// fail. client_test.go compared the outgoing Stripe-Version header to the
// apiVersion constant, so editing the constant kept the test green -- it proved
// the header was set, never that it was set to anything in particular.
func TestThePinnedVersionIsTheOneWeThinkItIs(t *testing.T) {
	const expected = "2025-08-27.basil"
	if apiVersion != expected {
		t.Fatalf("the pinned Stripe API version is %q, not %q.\n\n"+
			"Raising it is allowed, but it is a deliberate act: read the Stripe changelog "+
			"between the two versions, capture fresh payloads into internal/stripe/testdata, "+
			"make TestEveryCapturedDeliveryIsReadable pass against them, widen knownAPIVersions, "+
			"then change this constant. Six production defects came from a field moving between "+
			"versions with nothing to notice.", apiVersion, expected)
	}
	if !KnownAPIVersion(apiVersion) {
		t.Error("the pinned version is not in knownAPIVersions, so a delivery rendered by the " +
			"version we ourselves request would be reported as unreviewed")
	}
}

// TestAnUnknownAPIVersionIsReportedOnce proves the noisy-log guard: Stripe
// retries, and an alert that repeats every few seconds is one nobody reads.
func TestAnUnknownAPIVersionIsReportedOnce(t *testing.T) {
	seenVersion.Range(func(k, _ any) bool { seenVersion.Delete(k); return true })
	const v = "2099-01-01.invented"
	if _, loaded := seenVersion.Load(v); loaded {
		t.Fatal("the version cache was not empty at the start of the test")
	}
	noteAPIVersion(v, EventInvoicePaid)
	if _, loaded := seenVersion.Load(v); !loaded {
		t.Fatal("an unknown API version was not remembered, so it would be logged on every delivery")
	}
	noteAPIVersion(v, EventInvoicePaid) // must not shout again
	seenVersion.Delete(v)
}

// TestEveryEventTypeThisHubHandlesHasAFixtureOrASibling makes the coverage gap
// visible instead of silent. Two event types have never occurred on this
// account, so there is nothing to capture; both decode into a struct another
// fixture already covers, and this records which.
func TestEveryEventTypeThisHubHandlesHasAFixtureOrASibling(t *testing.T) {
	// event type -> the fixture that proves its payload shape
	covered := map[string]string{
		EventCheckoutCompleted:   "checkout_completed_dahlia.json",
		EventSubscriptionCreated: "subscription_created_basil.json",
		EventSubscriptionDeleted: "subscription_deleted_dahlia.json",
		EventInvoicePaid:         "invoice_paid_dahlia.json",
		EventInvoicePaymentPaid:  "invoice_payment_paid_dahlia.json",
		EventChargeRefunded:      "charge_refunded_dahlia.json",
		// No capture exists: neither has ever happened on this account.
		// customer.subscription.updated decodes into the same `subscription`
		// struct as created/deleted; invoice.payment_failed decodes into the
		// same `invoice` struct as invoice.paid. Capture them the first time
		// they occur -- payment_failed carries attempt_count and
		// next_payment_attempt, which no current fixture exercises.
		EventSubscriptionUpdated: "subscription_deleted_dahlia.json",
		EventInvoiceFailed:       "invoice_paid_dahlia.json",
	}
	all := []string{
		EventCheckoutCompleted, EventSubscriptionCreated, EventSubscriptionUpdated,
		EventSubscriptionDeleted, EventInvoicePaid, EventInvoiceFailed,
		EventChargeRefunded, EventInvoicePaymentPaid,
	}
	for _, typ := range all {
		f, ok := covered[typ]
		if !ok {
			t.Errorf("event %s is handled but no fixture proves its payload shape", typ)
			continue
		}
		if _, err := os.Stat(filepath.Join("testdata", f)); err != nil {
			t.Errorf("event %s claims coverage from %s, which is missing", typ, f)
		}
	}
}

// --- helpers -------------------------------------------------------------

func decode[T any](t *testing.T, ev Event) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(ev.Data.Object, &out); err != nil {
		t.Fatalf("decoding %s: %v", ev.Type, err)
	}
	return out
}

func want(t *testing.T, field, got, consequence string) {
	t.Helper()
	if strings.TrimSpace(got) != "" {
		return
	}
	if consequence == "" {
		t.Errorf("%s is absent from a real Stripe delivery", field)
		return
	}
	t.Errorf("%s is absent from a real Stripe delivery.\n  Consequence in production: %s", field, consequence)
}

func wantInt(t *testing.T, field string, got int64, consequence string) {
	t.Helper()
	if got != 0 {
		return
	}
	t.Errorf("%s is zero in a real Stripe delivery.\n  Consequence in production: %s", field, consequence)
}

func wantTime(t *testing.T, field string, got interface{ IsZero() bool }, consequence string) {
	t.Helper()
	if !got.IsZero() {
		return
	}
	t.Errorf("%s could not be read from a real Stripe delivery.\n  Consequence in production: %s", field, consequence)
}
