package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MESHSAT-1115. CreateChain validated only that a name was present and that
// there was at least one tier. A tier with no targets notifies nobody, and the
// engine cannot tell that apart from a tier whose delivery failed -- it counts
// a retry and walks on. So a chain that looked saved and paged no one was
// indistinguishable from a working one until the day it mattered.
//
// These also pin the shape the Escalation page must post. Before this, that
// page sent {delay_sec, recipients, actions}, which matches nothing in
// store.EscalationTier, and readJSON's DisallowUnknownFields answered 400 to
// every attempt: no tenant could configure an escalation at all.

func postChain(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewEscalationHandler(&mockStore{}, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/escalation/chains", strings.NewReader(body))
	h.CreateChain(rr, req)
	return rr
}

func TestAChainWhoseTierHasNoTargetsIsRefused(t *testing.T) {
	rr := postChain(t, `{"name":"silent","tiers":[{"name":"t1","targets":[],"wait_sec":0,"max_retries":1}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 -- a chain that notifies nobody must not look saved", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "notify nobody") {
		t.Errorf("body %q does not say why it was refused", rr.Body.String())
	}
}

func TestATierWhoseTargetsAreBlankIsRefused(t *testing.T) {
	rr := postChain(t, `{"name":"whitespace","tiers":[{"name":"t1","targets":["  "],"max_retries":1}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a whitespace-only target", rr.Code)
	}
}

// The second tier is the one a person adds last and is most likely to leave
// half-filled, so it must be checked too -- not just the first.
func TestALaterEmptyTierIsRefusedAndNamed(t *testing.T) {
	rr := postChain(t, `{"name":"half","tiers":[
		{"name":"t1","targets":["+31600000000"],"max_retries":1},
		{"name":"t2","targets":[],"wait_sec":300,"max_retries":1}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "tier 2") {
		t.Errorf("body %q should name which tier is at fault", rr.Body.String())
	}
}

// The shape the fixed UI posts must be accepted, or the fix is not a fix.
func TestTheShapeTheUIPostsIsAccepted(t *testing.T) {
	rr := postChain(t, `{"name":"crew on call","tiers":[
		{"name":"tier-1","targets":["+31600000000"],"wait_sec":0,"max_retries":1},
		{"name":"tier-2","targets":["+31600000001"],"wait_sec":300,"max_retries":2}]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "crew on call" {
		t.Errorf("name round-tripped as %v", got["name"])
	}
}

// The old UI's payload. Kept as a test so the regression is named rather than
// rediscovered: these field names must never be what the API expects.
func TestTheOldUIPayloadIsStillRejected(t *testing.T) {
	rr := postChain(t, `{"name":"old","tiers":[{"delay_sec":0,"recipients":["+31600000000"],"actions":["notify"]}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "delay_sec") {
		t.Errorf("body %q should name the unknown field, which is what makes this "+
			"diagnosable from a browser console", rr.Body.String())
	}
}
