package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// capture stands in for Stripe and records exactly what was sent.
func capture(t *testing.T, status int, body string) (*Client, *url.Values, *http.Header) {
	t.Helper()
	form := &url.Values{}
	hdr := &http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*form = r.PostForm
		*hdr = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("sk_test_notreal", 5*time.Second)
	c.SetBaseURL(srv.URL)
	return c, form, hdr
}

// Stripe Tax is off because there is no NL registration behind it, and the
// dashboard has a separate "use automatic tax" toggle that was found ON. Every
// request says so explicitly rather than trusting a setting somebody may flip.
func TestEveryCheckoutRefusesAutomaticTax(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://checkout.stripe.com/x"}`)

	if _, err := c.Checkout(context.Background(), CheckoutRequest{
		TenantID: "t1", PriceID: "price_crew", Email: "b@e.example",
		SuccessURL: "https://hub/ok", CancelURL: "https://hub/no",
	}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if got := form.Get("automatic_tax[enabled]"); got != "false" {
		t.Errorf("automatic_tax[enabled] = %q, want an explicit false", got)
	}
	if _, err := c.Donation(context.Background(), DonationRequest{
		PriceID: "price_donation", SuccessURL: "https://hub/ok", CancelURL: "https://hub/no",
	}); err != nil {
		t.Fatalf("donation: %v", err)
	}
	if got := form.Get("automatic_tax[enabled]"); got != "false" {
		t.Errorf("a donation did not refuse automatic tax: %q", got)
	}
}

// This is what replaces the claim code: the payment carries the tenant, on both
// the session and the subscription it creates, so every later event knows whose
// money it is without anybody typing anything.
func TestCheckoutBindsTheTenantToTheSessionAndTheSubscription(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://checkout.stripe.com/x"}`)

	if _, err := c.Checkout(context.Background(), CheckoutRequest{
		TenantID: "t-acme", PriceID: "price_crew",
		SuccessURL: "https://hub/ok", CancelURL: "https://hub/no",
	}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if got := form.Get("metadata[tenant_id]"); got != "t-acme" {
		t.Errorf("session metadata tenant = %q", got)
	}
	if got := form.Get("subscription_data[metadata][tenant_id]"); got != "t-acme" {
		t.Errorf("subscription metadata tenant = %q; without this, renewals arrive unattributed", got)
	}
	if got := form.Get("mode"); got != "subscription" {
		t.Errorf("mode = %q", got)
	}
	// The buyer's country decides whether Dutch VAT applies at all.
	if got := form.Get("billing_address_collection"); got != "required" {
		t.Errorf("billing_address_collection = %q; without it the receipt parks for lack of a country", got)
	}
}

// A returning subscriber must not become a second customer record with the same
// card — the billing system already has that problem in the other direction.
func TestCheckoutReusesAKnownCustomer(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{
		TenantID: "t1", PriceID: "price_crew", CustomerID: "cus_known", Email: "b@e.example",
	})
	if got := form.Get("customer"); got != "cus_known" {
		t.Errorf("customer = %q, want the one already known", got)
	}
	if form.Get("customer_email") != "" {
		t.Error("both a customer and an email were sent; Stripe would refuse or make a duplicate")
	}
}

// A retried create must not make a second object.
func TestWritesCarryAnIdempotencyKey(t *testing.T) {
	c, _, hdr := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "price_crew"})
	if hdr.Get("Idempotency-Key") == "" {
		t.Error("a checkout create carried no Idempotency-Key")
	}
}

// The API version is pinned so Stripe cannot change a payload shape underneath
// a running Hub.
func TestTheAPIVersionIsPinned(t *testing.T) {
	c, _, hdr := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "price_crew"})
	if got := hdr.Get("Stripe-Version"); got != apiVersion {
		t.Errorf("Stripe-Version = %q, want the pinned %q", got, apiVersion)
	}
}

func TestARefusalIsTypedAndSaysWhetherToRetry(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	} {
		c, _, _ := capture(t, tc.status, `{"error":{"message":"nope"}}`)
		_, err := c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "price_crew"})
		var se *Error
		if !errors.As(err, &se) {
			t.Fatalf("HTTP %d did not produce a *stripe.Error: %v", tc.status, err)
		}
		if se.Retryable() != tc.retryable {
			t.Errorf("HTTP %d: Retryable() = %v, want %v", tc.status, se.Retryable(), tc.retryable)
		}
	}
}

// A nil client is what an unconfigured Hub holds. It must report, not panic.
func TestAnUnconfiguredClientIsSafeToCall(t *testing.T) {
	if NewClient("", time.Second) != nil {
		t.Fatal("a client was built with no key")
	}
	var c *Client
	if _, err := c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "p"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Checkout: %v", err)
	}
	if _, err := c.Portal(context.Background(), "cus_1", "https://hub"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Portal: %v", err)
	}
	if c.Live() {
		t.Error("a nil client claims to be live")
	}
}

// Knowing which mode a key is in decides whether a test run can touch it.
func TestATestKeyIsNotLive(t *testing.T) {
	if NewClient("sk_test_abc", time.Second).Live() {
		t.Error("a test key reported itself live")
	}
	if !NewClient("sk_live_abc", time.Second).Live() {
		t.Error("a live key reported itself as test — the wrong way round to guess")
	}
	// Anything unrecognised is treated as live, because the other guess is the
	// one that spends real money.
	if !NewClient("rk_something_else", time.Second).Live() {
		t.Error("an unrecognised key was assumed safe")
	}
}

func TestPortalNeedsACustomer(t *testing.T) {
	c, _, _ := capture(t, 200, `{"id":"bps_1","url":"https://billing.stripe.com/x"}`)
	if _, err := c.Portal(context.Background(), "", "https://hub"); err == nil {
		t.Fatal("a portal session was opened for a tenant with no Stripe customer")
	}
	s, err := c.Portal(context.Background(), "cus_1", "https://hub")
	if err != nil || s.URL == "" {
		t.Fatalf("portal: %v %+v", err, s)
	}
}

func TestSubscriptionReadsBackWhatMatters(t *testing.T) {
	end := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]any{
		"id": "sub_1", "status": "active", "current_period_end": end.Unix(),
		"items": map[string]any{"data": []map[string]any{{"price": map[string]string{"id": "price_crew"}}}},
	})
	c, _, _ := capture(t, 200, string(body))
	status, periodEnd, price, err := c.Subscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if status != "active" || price != "price_crew" || !periodEnd.Equal(end) {
		t.Errorf("got %q / %v / %q", status, periodEnd, price)
	}
}

func TestCheckoutRefusesAnIncompleteRequest(t *testing.T) {
	c, _, _ := capture(t, 200, `{}`)
	for _, req := range []CheckoutRequest{
		{PriceID: "price_crew"},
		{TenantID: "t1"},
		{TenantID: "  ", PriceID: "  "},
	} {
		if _, err := c.Checkout(context.Background(), req); err == nil {
			t.Errorf("an incomplete request was sent to Stripe: %+v", req)
		}
	}
}

func TestTheSecretKeyIsSentAsABearerAndNeverInTheBody(t *testing.T) {
	c, form, hdr := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "price_crew"})
	if !strings.HasPrefix(hdr.Get("Authorization"), "Bearer sk_test_") {
		t.Errorf("Authorization = %q", hdr.Get("Authorization"))
	}
	for k, v := range *form {
		for _, s := range v {
			if strings.Contains(s, "sk_test_") {
				t.Errorf("the key leaked into the form as %s=%s", k, s)
			}
		}
	}
}

// Every session the Hub creates says what it is for. Without this a donation
// from somebody with no account is indistinguishable, at the webhook, from a
// payment that lost its tenant -- and one of the two would have to be guessed
// at. See apply.go's isAnonymousDonation.
func TestADonationSessionSaysItIsADonation(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://checkout.stripe.com/x"}`)

	if _, err := c.Donation(context.Background(), DonationRequest{
		PriceID: "price_gift", SuccessURL: "https://h/ok", CancelURL: "https://h/no",
	}); err != nil {
		t.Fatalf("Donation: %v", err)
	}
	if form.Get("metadata["+MetadataKind+"]") != KindDonation {
		t.Errorf("session carries no donation marker: %v", form)
	}
	if form.Get("mode") != "payment" {
		t.Errorf("mode = %q, want payment", form.Get("mode"))
	}
	// A gift buys nothing, so no tenant is required -- but the giver's country
	// still has to be collected, because the document is made out to them.
	if form.Get("billing_address_collection") != "required" {
		t.Errorf("billing address is not collected: %v", form)
	}
	if form.Get("metadata[tenant_id]") != "" {
		t.Errorf("an anonymous donation must not invent a tenant: %v", form)
	}
}

// A signed-in giver's donation carries both: the marker and their tenant.
func TestASignedInDonationCarriesTheTenantToo(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://checkout.stripe.com/x"}`)

	if _, err := c.Donation(context.Background(), DonationRequest{
		TenantID: "t_acme", PriceID: "price_gift",
		SuccessURL: "https://h/ok", CancelURL: "https://h/no",
	}); err != nil {
		t.Fatalf("Donation: %v", err)
	}
	if form.Get("metadata[tenant_id]") != "t_acme" {
		t.Errorf("tenant lost: %v", form)
	}
	if form.Get("metadata["+MetadataKind+"]") != KindDonation {
		t.Errorf("marker lost: %v", form)
// The hosted page's logo and title come from the ACCOUNT, and the account's own
// key cannot change them ("you may only use it on connected accounts"), so that
// half is a dashboard job. It is done: acct_1UEGj54j5c6KcLiz is MeshSat Hub's
// own, named and branded as such. This wording is the half the session controls,
// and it still earns its place by naming the PLAN being bought -- a customer who
// cannot tell what they are about to be charged for has a reason to stop.
func TestCheckoutSaysWhoIsChargingAndWhatFor(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{
		TenantID: "t1", PriceID: "price_crew", PlanName: "crew",
	})
	msg := form.Get("custom_text[submit][message]")
	if !strings.Contains(msg, "MeshSat Hub") {
		t.Errorf("the checkout page never names the product: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "crew") {
		t.Errorf("it does not say which plan is being bought: %q", msg)
	}
	// The two facts a customer most needs before paying.
	for _, want := range []string{"cancel", "VAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the wording omits %q: %q", want, msg)
		}
	}
}

// A missing plan name must not produce "MeshSat Hub ." or a panic on plan[:1].
func TestCheckoutWordingSurvivesAMissingPlanName(t *testing.T) {
	c, form, _ := capture(t, 200, `{"id":"cs_1","url":"https://x"}`)
	_, _ = c.Checkout(context.Background(), CheckoutRequest{TenantID: "t1", PriceID: "price_crew"})
	msg := form.Get("custom_text[submit][message]")
	if !strings.Contains(msg, "MeshSat Hub subscription") {
		t.Errorf("no sensible fallback wording: %q", msg)
	}
}
