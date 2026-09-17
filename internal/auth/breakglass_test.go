package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const bgToken = "break-glass-token-for-tests"

func bgHandler(t *testing.T, reached *bool) http.Handler {
	t.Helper()
	return Middleware(Config{Mode: "token", Token: bgToken})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*reached = true
			u := FromContext(r.Context())
			if u == nil || !u.PlatformAdmin {
				t.Errorf("handler reached without a platform-admin identity: %+v", u)
			}
		}))
}

// On the edge the token still works. This is the compatibility half: the
// nightly journey suite and the Stripe, refund and VAT tooling all depend on
// it, so removing it is not what this change does.
func TestBreakGlassStillWorksOnTheEdge(t *testing.T) {
	var reached bool
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+bgToken)
	rec := httptest.NewRecorder()
	bgHandler(t, &reached).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !reached {
		t.Fatal("handler not reached; the break-glass token must still authenticate at the edge")
	}
}

// On the onion it must NOT, and must be indistinguishable from a wrong token.
//
// The hidden service reaches the same router on its own listener, so a static
// master key was usable anonymously over Tor -- the one channel with no client
// identity to attribute a use to. No legitimate consumer needs it there.
func TestBreakGlassRefusedOnTheOnion(t *testing.T) {
	var reached bool
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+bgToken)
	// Mark the request as having arrived on the onion listener, the way
	// middleware.ClientIPContext does for a real one.
	req = req.WithContext(WithChannelLabel(req.Context(), "onion"))
	rec := httptest.NewRecorder()
	bgHandler(t, &reached).ServeHTTP(rec, req)

	if reached {
		t.Fatal("the break-glass token was honoured on the onion channel")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	// And the refusal must look exactly like a wrong token, so the onion
	// cannot be used as an oracle to confirm a guessed value.
	var wrongReached bool
	wrong := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	wrong.Header.Set("Authorization", "Bearer definitely-not-the-token")
	wrong = wrong.WithContext(WithChannelLabel(wrong.Context(), "onion"))
	wrec := httptest.NewRecorder()
	bgHandler(t, &wrongReached).ServeHTTP(wrec, wrong)

	if wrec.Code != rec.Code || wrec.Body.String() != rec.Body.String() {
		t.Errorf("a refused break-glass token is distinguishable from a wrong one:\n"+
			"  correct-token-on-onion: %d %q\n  wrong-token-on-onion   : %d %q",
			rec.Code, rec.Body.String(), wrec.Code, wrec.Body.String())
	}
}

// Both outcomes must reach the counter, and the series must exist before the
// first use so an alert has something to evaluate.
func TestBreakGlassUseIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, channel string
	}{
		{"accepted at the edge", "accepted", "internet"},
		{"refused on the onion", "refused_onion", "onion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lbl := map[string]string{"outcome": tc.outcome}
			before := counterValue(t, "meshsat_hub_breakglass_use_total", lbl)
			if before < 0 {
				t.Fatalf("meshsat_hub_breakglass_use_total{outcome=%q} has no series; "+
					"an alert on it could never fire", tc.outcome)
			}

			var reached bool
			req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
			req.Header.Set("Authorization", "Bearer "+bgToken)
			req = req.WithContext(WithChannelLabel(req.Context(), tc.channel))
			bgHandler(t, &reached).ServeHTTP(httptest.NewRecorder(), req)

			if after := counterValue(t, "meshsat_hub_breakglass_use_total", lbl); after != before+1 {
				t.Errorf("counter{outcome=%q} went %v -> %v, want +1", tc.outcome, before, after)
			}
		})
	}
}

// The audit sink is called with the acting identity and the request, for both
// outcomes. A master key whose use is not in the audit chain is a master key
// nobody can account for after the fact.
func TestBreakGlassIsAudited(t *testing.T) {
	type row struct{ action, actor, detail, ip string }
	var got []row
	SetBreakGlassAudit(func(_ context.Context, action, actor, detail, ip string) {
		got = append(got, row{action, actor, detail, ip})
	})
	defer SetBreakGlassAudit(nil)

	for _, ch := range []string{"internet", "onion"} {
		var reached bool
		req := httptest.NewRequest(http.MethodPost, "/api/admin/refunds", nil)
		req.Header.Set("Authorization", "Bearer "+bgToken)
		req = req.WithContext(WithChannelLabel(
			WithClientIP(req.Context(), "203.0.113.9"), ch))
		bgHandler(t, &reached).ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(got) != 2 {
		t.Fatalf("got %d audit rows, want 2 (one accepted, one refused): %+v", len(got), got)
	}
	if got[0].action != "breakglass_used" {
		t.Errorf("edge use recorded as %q, want breakglass_used", got[0].action)
	}
	if got[1].action != "breakglass_refused_onion" {
		t.Errorf("onion use recorded as %q, want breakglass_refused_onion", got[1].action)
	}
	for i, r := range got {
		if r.actor != "token-user" {
			t.Errorf("row %d actor = %q, want token-user", i, r.actor)
		}
		if r.ip != "203.0.113.9" {
			t.Errorf("row %d ip = %q, want the resolved client address", i, r.ip)
		}
		if r.detail == "" {
			t.Errorf("row %d has no detail; the row must say WHAT was reached", i)
		}
	}
}
