package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func bcryptCompare(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// provisionStatusOf calls the status endpoint the Fleet page polls.
func provisionStatusOf(t *testing.T, h *BridgeProvisionHandler, id string) (int, map[string]any, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/bridges/{id}/provision/status", h.ProvisionStatus)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/bridges/"+id+"/provision/status", nil))
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m, rr.Body.String()
}

// MESHSAT-1336. Generating a setup QR used to write the new password and
// certificate to the bridge row at once, and the broker took them up within
// a minute. A kit that was CONNECTED when its operator opened the QR was
// dropped on the reload and refused on every reconnect with its old
// password, whether or not the code was ever scanned. The first iPhone kit
// went offline that way on 24 Sep 2026 from a QR that was dismissed.
//
// The row is now written on the claim, and only then.

func TestAQRLeavesTheKitsLoginAloneUntilItIsScanned(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "kit-live")
	if n := len(m.credWrites); n != 0 {
		t.Fatalf("generating the QR wrote the bridge row %d time(s); the connected kit just lost its login", n)
	}
	if m.certWrites != 0 {
		t.Fatalf("generating the QR wrote the certificate %d time(s)", m.certWrites)
	}

	rr := claimBundle(t, h, "kit-live", nonce)
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	if n := len(m.credWrites); n != 1 {
		t.Fatalf("the claim wrote the bridge row %d time(s), want exactly 1", n)
	}
	if m.certWrites != 1 {
		t.Fatalf("the claim wrote the certificate %d time(s), want exactly 1", m.certWrites)
	}
	// What was written is what was handed out.
	var b ProvisionBundle
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Username != m.credWrites[0].user {
		t.Fatalf("row user %q, bundle user %q", m.credWrites[0].user, b.Username)
	}
	if err := bcryptCompare(m.credWrites[0].hash, b.Password); err != nil {
		t.Fatalf("the hash on the row is not the bundle's password: %v", err)
	}
}

// A QR that expires unscanned has changed nothing: no write ever happens,
// so the reaper removing the stash leaves the kit exactly as it was.
func TestAnExpiredQRNeverTouchesTheKitsLogin(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "kit-live")
	ageStash(t, m, "kit-live", ProvisionTTL+time.Minute)

	if rr := claimBundle(t, h, "kit-live", nonce); rr.Code != http.StatusGone {
		t.Fatalf("expired claim: %d", rr.Code)
	}
	if n := len(m.credWrites); n != 0 {
		t.Fatalf("an expired stash wrote the bridge row %d time(s)", n)
	}
}

// The claim is retried with the same nonce while the broker loads the new
// password (MESHSAT-1298). Every retry must not rewrite the row and kick the
// broker again, or the reload it is waiting for keeps being restarted.
func TestAClaimInstallsTheLoginOnceAcrossItsRetries(t *testing.T) {
	m, h := provisionFixture(t)
	broker := &fakeBroker{accepted: 1}
	h.SetProber(broker)
	nonce := stashOnly(t, h, "kit-a")

	for i := range 3 {
		if rr := claimBundle(t, h, "kit-a", nonce); rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("held claim %d: %d", i, rr.Code)
		}
	}
	if n := len(m.credWrites); n != 1 {
		t.Fatalf("three held claims wrote the row %d time(s), want 1", n)
	}
	broker.mu.Lock()
	broker.accepted = 3
	broker.mu.Unlock()
	if rr := claimBundle(t, h, "kit-a", nonce); rr.Code != http.StatusOK {
		t.Fatalf("claim once live: %d", rr.Code)
	}
	if n := len(m.credWrites); n != 1 {
		t.Fatalf("the successful claim wrote the row again: %d writes", n)
	}
}

// The direct endpoint hands the bundle back in its response, so the row is
// written in the same call: the caller asked for the new login and holds it.
func TestADirectProvisionInstallsAtOnce(t *testing.T) {
	m, h := provisionFixture(t)
	if rr := directProvision(t, h, "kit-d"); rr.Code != http.StatusOK {
		t.Fatalf("direct provision: %d", rr.Code)
	}
	if n := len(m.credWrites); n != 1 {
		t.Fatalf("direct provision wrote the row %d time(s), want 1", n)
	}
}

// The Fleet page shows the QR at once now: a stash nobody has scanned is
// `ready`, and the broker is not asked about a login it has not been given.
func TestAnUnscannedStashIsReadyAndAsksTheBrokerNothing(t *testing.T) {
	_, h := provisionFixture(t)
	broker := &fakeBroker{accepted: 0}
	h.SetProber(broker)
	stashOnly(t, h, "kit-a")
	_, st, body := provisionStatusOf(t, h, "kit-a")
	if st["state"] != "ready" {
		t.Fatalf("unscanned stash: %s", body)
	}
	if broker.calls != 0 {
		t.Fatalf("status asked the broker %d time(s) about a login it does not have", broker.calls)
	}
}

// A stash written before MESHSAT-1336 carries no hash: its credentials were
// installed when it was generated, and a claim hands it out without writing
// the row again.
func TestAStashFromBeforeTheChangeStillClaims(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "kit-old")
	key := provisionStashKey(store.DefaultTenantID, "kit-old")
	var stash provisionStash
	if err := json.Unmarshal([]byte(m.sysConfig[key]), &stash); err != nil {
		t.Fatal(err)
	}
	stash.PasswordHash = ""
	raw, _ := json.Marshal(stash)
	m.sysConfig[key] = string(raw)

	if rr := claimBundle(t, h, "kit-old", nonce); rr.Code != http.StatusOK {
		t.Fatalf("legacy claim: %d %s", rr.Code, rr.Body.String())
	}
	if n := len(m.credWrites); n != 0 {
		t.Fatalf("a legacy stash wrote the row %d time(s)", n)
	}
}

// ageStash backdates the bridge's stash by d.
func ageStash(t *testing.T, m *mockStore, id string, d time.Duration) {
	t.Helper()
	key := provisionStashKey(store.DefaultTenantID, id)
	var stash provisionStash
	if err := json.Unmarshal([]byte(m.sysConfig[key]), &stash); err != nil {
		t.Fatal(err)
	}
	stash.CreatedAt = stash.CreatedAt.Add(-d)
	raw, _ := json.Marshal(stash)
	m.sysConfig[key] = string(raw)
}
