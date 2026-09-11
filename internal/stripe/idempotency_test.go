package stripe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The Subscribe button stopped working for a whole day, per customer, per plan.
//
// The checkout idempotency key was tenant+price and nothing else. Stripe
// remembers a key for 24 hours and REFUSES it when the parameters differ, so a
// customer who opened Checkout, wandered off and pressed Subscribe again the
// same day got back either the first session -- expired by then -- or a flat
// 400 the Hub reported as "the payment provider could not be reached".
//
// Caught by stripe-live-check.py against the live account:
//
//	idempotency_error: Keys for idempotent requests can only be used with the
//	same parameters they were first used with. Try using a key other than
//	'meshsat-checkout-t-stripe-live-price_1UEHEb...'
//
// A key is meant to make a RETRY safe, not to stop a second attempt existing.

func keyFor(t *testing.T, c *Client, tenant, price string) string {
	t.Helper()
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_1","url":"https://checkout.stripe.com/c/pay/cs_1"}`))
	}))
	defer srv.Close()
	c.SetBaseURL(srv.URL)
	if _, err := c.Checkout(context.Background(), CheckoutRequest{
		TenantID: tenant, PriceID: price, Email: "a@example.org",
		SuccessURL: "https://hub.example/ok", CancelURL: "https://hub.example/no",
	}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	return key
}

func TestARetriedCheckoutReusesTheKeyButALaterAttemptDoesNot(t *testing.T) {
	c := NewClient("sk_test_x", 5*time.Second)
	base := time.Date(2026, 9, 11, 10, 44, 0, 0, time.UTC)

	// A double click: seconds apart, same key, so Stripe hands back the same
	// session instead of opening a second one.
	c.now = func() time.Time { return base }
	first := keyFor(t, c, "t1", "price_crew")
	c.now = func() time.Time { return base.Add(3 * time.Second) }
	doubleClick := keyFor(t, c, "t1", "price_crew")
	if first == "" || first != doubleClick {
		t.Fatalf("a double click produced keys %q and %q; it should dedupe", first, doubleClick)
	}

	// Genuinely starting again later. A NEW key, or the customer gets an
	// expired session or a 400 for the rest of the day.
	c.now = func() time.Time { return base.Add(40 * time.Minute) }
	later := keyFor(t, c, "t1", "price_crew")
	if later == first {
		t.Fatalf("a second attempt reused key %q; Subscribe would fail for 24h", first)
	}

	// And the next day, certainly not.
	c.now = func() time.Time { return base.Add(25 * time.Hour) }
	if tomorrow := keyFor(t, c, "t1", "price_crew"); tomorrow == first {
		t.Fatal("the key is stable across a day")
	}
}

// Two tenants, or two plans, must never share a key whatever the clock says.
func TestCheckoutKeysDoNotCollideAcrossTenantsOrPlans(t *testing.T) {
	c := NewClient("sk_test_x", 5*time.Second)
	at := time.Date(2026, 9, 11, 10, 44, 0, 0, time.UTC)
	c.now = func() time.Time { return at }

	a := keyFor(t, c, "t1", "price_crew")
	b := keyFor(t, c, "t2", "price_crew")
	d := keyFor(t, c, "t1", "price_fleet")
	if a == b {
		t.Error("two tenants share one checkout key")
	}
	if a == d {
		t.Error("two plans share one checkout key")
	}
}

// An anonymous donation has no tenant, so a bucket would BE the key and two
// strangers giving in the same quarter hour would be handed each other's
// session. Donations stay unique per call.
func TestAnonymousDonationsNeverShareAKey(t *testing.T) {
	c := NewClient("sk_test_x", 5*time.Second)
	at := time.Date(2026, 9, 11, 10, 44, 0, 0, time.UTC)
	c.now = func() time.Time { return at }

	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_d","url":"https://checkout.stripe.com/c/pay/cs_d"}`))
	}))
	defer srv.Close()
	c.SetBaseURL(srv.URL)

	for i := 0; i < 2; i++ {
		if _, err := c.Donation(context.Background(), DonationRequest{
			PriceID: "price_donation", SuccessURL: "https://hub.example/ta",
			CancelURL: "https://hub.example/no",
		}); err != nil {
			t.Fatalf("Donation: %v", err)
		}
	}
	if keys[0] == keys[1] {
		t.Fatalf("two anonymous givers shared key %q; one would be handed the other's session", keys[0])
	}
}
