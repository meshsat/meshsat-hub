package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/stripe"
)

// The first real anonymous donation returned the giver to
// https://hub.meshsat.net/#/login?redirect=/?donation=thanks -- a sign-in wall,
// shown to somebody who had just paid and has no account and never will. The
// success URL pointed at "/#/", every SPA route carries requiresAuth, and the
// router did what it is supposed to do.
//
// These tests hold the return where it belongs: on a page the Hub serves to
// anybody.

// stripeDouble captures the form the Hub would send Stripe and answers with a
// plausible session.
func stripeDouble(t *testing.T, got *url.Values) *stripe.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing the form Stripe was sent: %v", err)
		}
		*got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`))
	}))
	t.Cleanup(srv.Close)
	c := stripe.NewClient("sk_test_x", 5*time.Second)
	c.SetBaseURL(srv.URL)
	return c
}

func TestTheAnonymousDonationReturnsToAPageNotToTheApp(t *testing.T) {
	var form url.Values
	h := NewBillingHandler(&mockStore{}, stripeDouble(t, &form), "https://hub.meshsat.net")
	h.SetDonationPrice("price_donation")

	w := httptest.NewRecorder()
	h.DonateRedirect(w, httptest.NewRequest(http.MethodGet, "/donate", nil))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", w.Code)
	}
	for _, field := range []string{"success_url", "cancel_url"} {
		u := form.Get(field)
		if u == "" {
			t.Fatalf("%s was not sent to Stripe", field)
		}
		// The defect, named: a "/#/" destination is a client-side route, every
		// one of them requires auth, and a giver with no account lands on the
		// sign-in page holding a receipt.
		if strings.Contains(u, "/#/") {
			t.Errorf("%s = %q sends a giver with no account into the SPA", field, u)
		}
		if !strings.HasPrefix(u, "https://hub.meshsat.net/donate/") {
			t.Errorf("%s = %q, want one of the public donate pages", field, u)
		}
	}
	if got := form.Get("success_url"); got != "https://hub.meshsat.net/donate/thanks" {
		t.Errorf("success_url = %q", got)
	}
	if got := form.Get("cancel_url"); got != "https://hub.meshsat.net/donate/cancelled" {
		t.Errorf("cancel_url = %q", got)
	}
}

// A signed-in owner donating from Settings is a different person with a
// different destination, and that one was never broken.
func TestATenantDonationStillReturnsToSettings(t *testing.T) {
	var form url.Values
	h := NewBillingHandler(&mockStore{}, stripeDouble(t, &form), "https://hub.meshsat.net")
	h.SetDonationPrice("price_donation")

	w := httptest.NewRecorder()
	h.donate(w, httptest.NewRequest(http.MethodPost, "/api/tenant/billing/donate", nil),
		"t-a", "owner@example.com")

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := form.Get("success_url"); !strings.Contains(got, "/#/settings") {
		t.Errorf("success_url = %q, want the Settings page", got)
	}
}

func TestTheReturnPagesRenderForSomebodyWithNoAccount(t *testing.T) {
	h := NewBillingHandler(&mockStore{}, nil, "https://hub.meshsat.net")

	for _, tc := range []struct {
		name    string
		serve   func(http.ResponseWriter, *http.Request)
		must    []string
		mustNot []string
	}{
		{
			name:  "thanks",
			serve: h.DonateThanks,
			must: []string{
				"Thank you.",
				"receipt is on its way",
				"carries no VAT",   // why the document shows none
				"unlocks nothing",  // it bought no tier
				"needs no account", // and there is nothing to sign in to
				"https://meshsat.net",
			},
		},
		{
			name:  "cancelled",
			serve: h.DonateCancelled,
			must: []string{
				"Nothing was charged.",
				"no money moved",
				"https://meshsat.net",
			},
			// Abandoning a payment is a choice, not a fault.
			mustNot: []string{"error", "failed", "sorry", "Sorry"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.serve(w, httptest.NewRequest(http.MethodGet, "/donate/"+tc.name, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("content type %q", ct)
			}
			// Transactional and per-person: never held by a proxy.
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}

			body := w.Body.String()
			for _, want := range tc.must {
				if !strings.Contains(body, want) {
					t.Errorf("the page does not say %q", want)
				}
			}
			for _, no := range tc.mustNot {
				if strings.Contains(body, no) {
					t.Errorf("the page reads as a failure: %q", no)
				}
			}

			// It renders standalone: no SPA bundle, no script, no webfont, and
			// nothing fetched from a third party. The only asset is the brand
			// mark, which is already public.
			for _, forbidden := range []string{"<script", "/assets/", "fonts.googleapis", "http://"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("the page pulls in %q; it must stand alone", forbidden)
				}
			}
			// Both themes, because there is no session here to remember one in.
			if !strings.Contains(body, "prefers-color-scheme:dark") {
				t.Error("the page has no dark theme")
			}
			// A thank-you page is not a search result.
			if !strings.Contains(body, `name="robots" content="noindex"`) {
				t.Error("the page is indexable")
			}
		})
	}
}
