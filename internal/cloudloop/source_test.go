package cloudloop

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestExpectedSourcesUnderstandsAddressesAndRanges(t *testing.T) {
	e := parseExpectedSources([]string{"18.130.0.0/16", "35.176.1.2", " ", "*", "not-an-address"})
	if !e.configured() {
		t.Fatal("nothing parsed")
	}
	cases := map[string]bool{
		"18.130.44.7": true,  // inside the CIDR
		"35.176.1.2":  true,  // the exact address
		"35.176.1.3":  false, // one along from it
		"8.8.8.8":     false,
		"":            false,
		"garbage":     false,
	}
	for ip, want := range cases {
		if got := e.contains(ip); got != want {
			t.Errorf("contains(%q) = %v, want %v", ip, got, want)
		}
	}
	// "*" must not become a range that matches everything.
	if parseExpectedSources([]string{"*"}).configured() {
		t.Error(`"*" was parsed as a source range; it means "no opinion", not "match all"`)
	}
}

// The whole point of this control. Cloudloop publishes no egress addresses, so
// any range here is a guess -- and this is the path a satellite MO message
// arrives on, which is the path an SOS arrives on. Observation must never be
// able to refuse one.
func TestAnUnexpectedSourceIsRecordedAndStillProcessed(t *testing.T) {
	h := &WebhookHandler{allowedIPs: []string{"*"}, token: "t"}
	h.SetExpectedSources([]string{"18.130.0.0/16"})

	r := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", nil)
	r.RemoteAddr = "203.0.113.9:443" // nowhere near the expected range

	// noteSource returns nothing on purpose: there is no decision for a caller
	// to accidentally branch on. If this ever grows a return value, the next
	// person will gate on it and an SOS will be dropped.
	h.noteSource(r, "cloudloop")

	// And the blocking control is untouched by any of it.
	if !h.wildcardAllowlist() {
		t.Error("observation changed the allowlist; it must not touch what gates traffic")
	}
}

func TestNoExpectedRangeMeansNoOpinion(t *testing.T) {
	h := &WebhookHandler{allowedIPs: []string{"*"}}
	r := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", nil)
	r.RemoteAddr = "203.0.113.9:443"
	h.noteSource(r, "cloudloop") // must not panic or count anything
	if h.expected.configured() {
		t.Error("an unconfigured observer reports itself configured")
	}
}

// A header anyone can set is not evidence. X-Forwarded-For is honoured only
// from a proxy already trusted, exactly as the blocking allowlist does it.
func TestAForgedForwardedHeaderDoesNotMoveTheSource(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", nil)
	r.RemoteAddr = "203.0.113.9:443" // not a trusted proxy
	r.Header.Set("X-Forwarded-For", "18.130.44.7")
	if got := sourceIP(r); got != "203.0.113.9" {
		t.Errorf("sourceIP = %q; a caller claiming to be someone else was believed", got)
	}
}

// A structural guard, in the shape internal/quota and internal/stripe already
// use for the SOS invariant: read the source and fail if the observation has
// been turned into a decision.
//
// The danger is not today's code, it is the reasonable-looking change six
// months from now -- "we know the range, why are we letting strangers in?" --
// made by somebody who has not read why the allowlist is "*". The answer is
// that Cloudloop publishes no egress addresses, so the range is a guess, and
// the traffic is the path an SOS arrives on.
func TestObservingASourceCannotBecomeARefusal(t *testing.T) {
	src, err := os.ReadFile("source.go")
	if err != nil {
		t.Fatalf("reading source.go: %v", err)
	}
	text := string(src)

	// noteSource must stay a statement, not an expression.
	if !strings.Contains(text, "func (h *WebhookHandler) noteSource(r *http.Request, provider string) {") {
		t.Error("noteSource's signature changed; if it now returns something, a caller can gate on it")
	}
	for _, forbidden := range []string{
		"return h.expected.contains",
		"http.Error",
		"StatusForbidden",
		"WriteHeader",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("source.go contains %q: observing where a webhook came from must never "+
				"refuse it. This is the inbound satellite path.", forbidden)
		}
	}

	// And the handler must not branch on it.
	wh, err := os.ReadFile("webhook.go")
	if err != nil {
		t.Fatalf("reading webhook.go: %v", err)
	}
	for _, forbidden := range []string{"if h.noteSource(", "!h.noteSource(", "= h.noteSource("} {
		if strings.Contains(string(wh), forbidden) {
			t.Errorf("webhook.go branches on noteSource (%q); it is an observation, not a gate", forbidden)
		}
	}
}
