package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/bridge"
)

// fakeBroker answers the prober like three NATS members, of which `accepted`
// already accept the credential.
type fakeBroker struct {
	mu       sync.Mutex
	accepted int
	calls    int
}

func (f *fakeBroker) Live(_ context.Context, _, _ string) (bool, []bridge.ProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	res := make([]bridge.ProbeResult, 3)
	for i := range res {
		res[i] = bridge.ProbeResult{Addr: "10.0.0." + string(rune('1'+i)), Accepted: i < f.accepted}
	}
	return f.accepted == 3, res, nil
}

// MESHSAT-1298. A phone that claimed its bundle and connected 8 s after the QR
// was generated was refused with the right password, because the broker
// members load a new credential up to a minute later. The claim now answers
// "not yet" and keeps the stash until every member accepts it.
func TestAClaimWaitsUntilTheBrokerAcceptsTheCredentials(t *testing.T) {
	m, h := provisionFixture(t)
	broker := &fakeBroker{accepted: 1}
	h.SetProber(broker)
	nonce := stashOnly(t, h, "kit-a")

	rr := claimBundle(t, h, "kit-a", nonce)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("claim before the broker accepts: %d, Retry-After %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if len(provisionStashKeys(m)) != 1 {
		t.Fatal("a held claim burned the stash; the same nonce must work a few seconds later")
	}

	broker.mu.Lock()
	broker.accepted = 3
	broker.mu.Unlock()
	rr = claimBundle(t, h, "kit-a", nonce)
	if rr.Code != http.StatusOK {
		t.Fatalf("claim once live: %d %s", rr.Code, rr.Body.String())
	}
	if len(provisionStashKeys(m)) != 0 {
		t.Fatal("a delivered bundle left its stash behind")
	}
}

// A prober that cannot answer must not block every provisioning: the bundle is
// handed out unchecked, as it was before.
type brokenBroker struct{}

func (brokenBroker) Live(context.Context, string, string) (bool, []bridge.ProbeResult, error) {
	return false, nil, context.DeadlineExceeded
}

func TestAClaimIsNotHeldWhenTheBrokerCannotBeAsked(t *testing.T) {
	_, h := provisionFixture(t)
	h.SetProber(brokenBroker{})
	nonce := stashOnly(t, h, "kit-a")
	if rr := claimBundle(t, h, "kit-a", nonce); rr.Code != http.StatusOK {
		t.Fatalf("an unreachable prober blocked the claim: %d", rr.Code)
	}
}

func TestProvisionStatusReportsCountsNotAddresses(t *testing.T) {
	_, h := provisionFixture(t)
	broker := &fakeBroker{accepted: 2}
	h.SetProber(broker)
	status := func() (int, map[string]any, string) {
		r := chi.NewRouter()
		r.Get("/api/bridges/{id}/provision/status", h.ProvisionStatus)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/bridges/kit-a/provision/status", nil))
		var m map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &m)
		return rr.Code, m, rr.Body.String()
	}
	if code, m, _ := status(); code != 200 || m["state"] != "none" {
		t.Fatalf("no stash: %d %v", code, m)
	}
	nonce := stashOnly(t, h, "kit-a")
	// Unscanned: the broker has not been given the login, so nothing is
	// pending at it (MESHSAT-1336). The first claim installs it.
	if code, m, body := status(); code != 200 || m["state"] != "ready" {
		t.Fatalf("unscanned: %d %s", code, body)
	}
	if rr := claimBundle(t, h, "kit-a", nonce); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("claim with 2 of 3 members: %d", rr.Code)
	}
	code, m, body := status()
	if code != 200 || m["state"] != "pending" || m["accepted"] != float64(2) || m["members"] != float64(3) {
		t.Fatalf("pending: %d %s", code, body)
	}
	for _, leak := range []string{"10.0.0.", nonce, "\"pass\""} {
		if strings.Contains(body, leak) {
			t.Fatalf("status leaks %q: %s", leak, body)
		}
	}
	broker.mu.Lock()
	broker.accepted = 3
	broker.mu.Unlock()
	if _, m, _ := status(); m["state"] != "live" {
		t.Fatalf("live: %v", m)
	}
}
