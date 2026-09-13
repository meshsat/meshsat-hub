package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Bridge provisioning had NO tests at all -- the TTL, the nonce comparison, the
// single-use property and the blanking were entirely uncovered, which is how a
// direct provision kept its stash for six months without anybody noticing
// (MESHSAT-1098). These are modelled on the TAK enrolment suite next door,
// which covers the same shape and did have them.

func provisionStashKeys(m *mockStore) []string {
	var out []string
	for k, v := range m.sysConfig {
		if strings.HasPrefix(k, "provision_stash:") && v != "" {
			out = append(out, k)
		}
	}
	return out
}

func claimBundle(t *testing.T, h *BridgeProvisionHandler, id, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/bridges/{id}/provision/{nonce}", h.ClaimProvision)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/bridges/"+id+"/provision/"+nonce, nil))
	return rr
}

// The defect this whole change exists for. The direct endpoint handed the
// bundle back in its response and left the stash in place, so the plaintext
// MQTT password and the client PRIVATE KEY stayed in system_config -- and in
// every unencrypted backup -- for as long as the bridge existed.
func TestADirectProvisionDoesNotLeaveTheBundleBehind(t *testing.T) {
	m, h := provisionFixture(t)
	rr := directProvision(t, h, "b1")
	if rr.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rr.Code, rr.Body.String())
	}
	// The caller got the bundle...
	var bundle map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if bundle["key"] == "" || bundle["key"] == nil {
		t.Fatal("the response carried no private key, so this test is not exercising the thing")
	}
	// ...and nothing was left in the table.
	if left := provisionStashKeys(m); len(left) != 0 {
		t.Errorf("a live stash survived a direct provision: %v\n"+
			"It holds the plaintext MQTT password and the client private key, and "+
			"system_config goes into every barman backup.", left)
	}
}

func TestClaimingDeliversTheBundleExactlyOnce(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "b1")

	rr := claimBundle(t, h, "b1", nonce)
	if rr.Code != http.StatusOK {
		t.Fatalf("first claim: %d %s", rr.Code, rr.Body.String())
	}
	if left := provisionStashKeys(m); len(left) != 0 {
		t.Errorf("the stash survived a claim: %v", left)
	}
	// A second claim finds nothing, and says so the same way an unknown bridge does.
	again := claimBundle(t, h, "b1", nonce)
	if again.Code != http.StatusNotFound {
		t.Errorf("second claim: %d, want 404", again.Code)
	}
}

// A failed claim must not consume the enrolment -- otherwise one wrong scan
// burns a provisioning session that is still good.
func TestAWrongNonceDoesNotBurnTheStash(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "b1")

	rr := claimBundle(t, h, "b1", strings.Repeat("0", len(nonce)))
	if rr.Code != http.StatusNotFound {
		t.Errorf("wrong nonce: %d, want 404", rr.Code)
	}
	if left := provisionStashKeys(m); len(left) != 1 {
		t.Fatalf("a wrong nonce consumed the stash: %v", left)
	}
	if ok := claimBundle(t, h, "b1", nonce); ok.Code != http.StatusOK {
		t.Errorf("the right nonce afterwards: %d, want 200", ok.Code)
	}
}

// The two refusals must be indistinguishable, or an unauthenticated caller can
// enumerate which bridge ids have a session in flight.
func TestAnUnknownBridgeLooksExactlyLikeAWrongNonce(t *testing.T) {
	_, h := provisionFixture(t)
	nonce := stashOnly(t, h, "b1")

	wrong := claimBundle(t, h, "b1", strings.Repeat("f", len(nonce)))
	unknown := claimBundle(t, h, "b-does-not-exist", nonce)

	if wrong.Code != unknown.Code {
		t.Errorf("status differs: wrong nonce %d, unknown bridge %d", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("body differs:\n  wrong nonce  : %s\n  unknown bridge: %s",
			wrong.Body.String(), unknown.Body.String())
	}
}

// The TTL was a bare literal at its single enforcement point, so nothing could
// reach it. Now it is ProvisionTTL and this can.
func TestAnExpiredStashIsRefusedAndCleared(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "b1")

	key := "provision_stash:b1"
	var st provisionStash
	if err := json.Unmarshal([]byte(m.sysConfig[key]), &st); err != nil {
		t.Fatalf("stash: %v", err)
	}
	st.CreatedAt = time.Now().Add(-ProvisionTTL - time.Minute)
	raw, _ := json.Marshal(st)
	m.sysConfig[key] = string(raw)

	rr := claimBundle(t, h, "b1", nonce)
	if rr.Code != http.StatusGone {
		t.Errorf("expired claim: %d, want 410", rr.Code)
	}
	if left := provisionStashKeys(m); len(left) != 0 {
		t.Errorf("an expired stash was left in place: %v", left)
	}
}

// Re-provisioning replaces the stash rather than accumulating one per attempt,
// which is what stops an operator retrying a QR from leaving a trail of live
// bundles behind.
func TestReprovisioningReplacesTheStashRatherThanAddingOne(t *testing.T) {
	m, h := provisionFixture(t)
	first := stashOnly(t, h, "b1")
	second := stashOnly(t, h, "b1")
	if first == second {
		t.Fatal("the nonce did not change, so this proves nothing")
	}
	if left := provisionStashKeys(m); len(left) != 1 {
		t.Fatalf("stashes after two provisions: %v, want exactly one", left)
	}
	if rr := claimBundle(t, h, "b1", first); rr.Code != http.StatusNotFound {
		t.Errorf("the superseded nonce still claims: %d, want 404", rr.Code)
	}
}

// --- fixture ---

func provisionFixture(t *testing.T) (*mockStore, *BridgeProvisionHandler) {
	t.Helper()
	m := &mockStore{
		sysConfig: map[string]string{"mqtt_public_url": "wss://mqtt-hub.test/mqtt"},
		bridge:    &store.Bridge{TenantID: store.DefaultTenantID},
	}
	ca, _, _, err := bridge.NewSelfSignedCA("test")
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	anchor, err := directory.LoadOrCreateTrustAnchor(context.Background(), m)
	if err != nil {
		t.Fatalf("trust anchor: %v", err)
	}
	return m, NewBridgeProvisionHandler(m, ca, anchor)
}

// directProvision calls the one-step endpoint, which returns the bundle inline.
func directProvision(t *testing.T, h *BridgeProvisionHandler, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/provision", h.Provision)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/bridges/"+id+"/provision", nil))
	return rr
}

// stashOnly creates a stash the way the QR path does and returns its nonce,
// without consuming it.
func stashOnly(t *testing.T, h *BridgeProvisionHandler, id string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	nonce, err := h.generateAndStash(req, id, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("generateAndStash: %v", err)
	}
	return nonce
}

// The nonce IS the credential on an unauthenticated endpoint, so it must be
// compared in constant time. No behavioural test can see this -- `!=` and
// ConstantTimeCompare accept and reject exactly the same inputs -- so it is
// pinned by reading the source, the way internal/quota pins its invariants.
// Proved necessary by mutating the comparison back to `!=` and watching every
// other test in this file stay green.
func TestTheNonceIsComparedInConstantTime(t *testing.T) {
	src, err := os.ReadFile("bridge_provision.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "subtle.ConstantTimeCompare([]byte(stash.Nonce), []byte(nonce))") {
		t.Error("the claim path no longer compares the nonce with subtle.ConstantTimeCompare; " +
			"this endpoint is unauthenticated and the nonce is the only thing guarding it")
	}
	if strings.Contains(s, "stash.Nonce != nonce") || strings.Contains(s, "nonce != stash.Nonce") {
		t.Error("a direct string comparison of the nonce is back")
	}
}
