package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The page loosens the Content-Security-Policy to admit js.stripe.com. That is
// the cost of embedding, and it must be paid on this route ONLY -- the rest of
// the Hub keeps script-src 'self'. These tests are the thing standing between
// "one payment page trusts Stripe" and "the whole application does".

func TestTheDonationPolicyAdmitsStripeAndNothingElse(t *testing.T) {
	csp := cspWithNonce("TESTNONCE")

	for _, want := range []string{
		"default-src 'self'",
		"https://js.stripe.com",
		"'nonce-TESTNONCE'",
		"frame-ancestors 'none'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("the donation policy is missing %q", want)
		}
	}

	// 'unsafe-inline' in script-src would make the nonce pointless and let an
	// injected <script> run on the page that handles money.
	script := directive(t, csp, "script-src")
	if strings.Contains(script, "'unsafe-inline'") {
		t.Error("script-src carries 'unsafe-inline'; the nonce then protects nothing")
	}
	if strings.Contains(script, "'unsafe-eval'") {
		t.Error("script-src carries 'unsafe-eval'")
	}

	// Only Stripe. A wildcard here would admit any origin that talks a browser.
	for _, d := range []string{"script-src", "frame-src", "connect-src"} {
		v := directive(t, csp, d)
		for _, tok := range strings.Fields(v) {
			if !strings.HasPrefix(tok, "http") {
				continue // 'self', 'nonce-...', the directive name
			}
			if !strings.HasPrefix(tok, "https://") {
				t.Errorf("%s admits a non-https origin %q", d, tok)
			}
			if !strings.Contains(tok, "stripe.com") {
				t.Errorf("%s admits %q, which is not Stripe", d, tok)
			}
		}
	}
}

// The loosened policy must not leak onto any other route.
func TestOnlyTheDonationPageRelaxesThePolicy(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tenant", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "stripe.com") {
		t.Error("the global policy admits Stripe; embedding was supposed to be scoped to /donate")
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("the global script-src is no longer just 'self': %q", csp)
	}
}

// A donor must never meet a payment box that cannot work. With no publishable
// key the page has to hand over to the hosted flow rather than render a form
// Stripe.js will refuse to mount.
func TestWithNoPublishableKeyTheDonationFallsBackToHosted(t *testing.T) {
	h := &BillingHandler{donatio: "price_x", client: nil}
	rec := httptest.NewRecorder()
	h.DonatePage(rec, httptest.NewRequest(http.MethodGet, "/donate", nil))
	// A nil client means donations are off entirely; that is the 503 branch.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when donations are unconfigured", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "charged") == false {
		t.Error("the unavailable page should say plainly that nothing was charged")
	}
}

// Every nonce must be fresh. A reused one is a nonce in name only.
func TestEveryRenderGetsItsOwnNonce(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n, err := nonceFor()
		if err != nil {
			t.Fatalf("nonceFor: %v", err)
		}
		if len(n) < 16 {
			t.Fatalf("nonce %q is too short to be worth having", n)
		}
		if seen[n] {
			t.Fatalf("nonce %q was issued twice", n)
		}
		seen[n] = true
	}
}

// The page states the tax position, because a donation is outside the scope of
// BTW and a giver should not have to wonder why there is no VAT line.
func TestThePageSaysWhatTheMoneyIsAndIsNot(t *testing.T) {
	src := donateEmbedTmplSource()
	for _, want := range []string{"gift", "no VAT", "receipt", "unlocks nothing"} {
		if !strings.Contains(src, want) {
			t.Errorf("the donation page never says %q", want)
		}
	}
	// Deliberately NOT the funder credit. Telling somebody you are already
	// grant-funded, on the page where you ask them for money, argues against
	// the ask. It belongs on the org profile, not here (owner ruling).
	if strings.Contains(src, "sidnfonds") {
		t.Error("the donation page credits a funder; that undercuts the ask and was removed on purpose")
	}
}

func directive(t *testing.T, csp, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?:^|;\s*)` + regexp.QuoteMeta(name) + `\s+([^;]*)`)
	m := re.FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("policy has no %s directive: %q", name, csp)
	}
	return m[1]
}
