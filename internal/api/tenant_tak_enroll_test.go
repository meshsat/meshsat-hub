package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Enrolment (MESHSAT-1040). What these tests are for: the claim endpoint is
// UNAUTHENTICATED, so its refusals, its single-use property and the fact that a
// stolen database row is not an enrollable identity are the whole security story.
// None of that is visible from a green build.

// takCertFixture is a real key and a real certificate chain, because the handler
// hands them to takenroll, which parses them and checks the CN. A stub PEM would
// make every test pass against a code path that cannot work.
type takCertFixture struct {
	keyPEM  []byte
	certPEM []byte
	caPEM   []byte
}

func newTAKCertFixture(t *testing.T, username string) takCertFixture {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tenant CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: username},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return takCertFixture{
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		caPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}
}

type fakeTAKCerts struct {
	fx         takCertFixture
	requested  []string
	discarded  []string
	outcome    TAKCertOutcome
	msg        string
	collectErr error
	reqErr     error
}

func (f *fakeTAKCerts) NewKey(string) (csrPEM, keyPEM []byte, err error) {
	// The same key the fixture's certificate was issued for, so the pair the
	// handler assembles is genuinely coherent.
	return []byte("-----BEGIN CERTIFICATE REQUEST-----\nx\n-----END CERTIFICATE REQUEST-----\n"), f.fx.keyPEM, nil
}

func (f *fakeTAKCerts) Request(_ context.Context, label, username string, _ []byte) error {
	f.requested = append(f.requested, label+"/"+username)
	return f.reqErr
}

func (f *fakeTAKCerts) Discard(_ context.Context, label, username string) error {
	f.discarded = append(f.discarded, label+"/"+username)
	return nil
}

func (f *fakeTAKCerts) Collect(_ context.Context, _, _ string) (TAKCertMaterial, TAKCertOutcome, string, error) {
	if f.collectErr != nil {
		return TAKCertMaterial{}, f.outcome, f.msg, f.collectErr
	}
	if f.outcome != TAKCertReady {
		return TAKCertMaterial{}, f.outcome, f.msg, nil
	}
	return TAKCertMaterial{
		CertPEM:   string(f.fx.certPEM),
		CACertPEM: string(f.fx.caPEM),
		Serial:    "0A0B0C",
		NotAfter:  time.Now().Add(90 * 24 * time.Hour),
	}, TAKCertReady, "", nil
}

// apiFrontTrust is the chain of the certificate the TAK front presents -- a
// DIFFERENT certificate from the tenant CA, which is the whole point: the
// truststore in a package must carry this one. Generated once; two RSA keys per
// test is pure cost.
var (
	apiFrontOnce sync.Once
	apiFrontPEM  []byte
)

func apiFrontTrust(t *testing.T) []byte {
	t.Helper()
	apiFrontOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(500),
			Subject:               pkix.Name{CommonName: "Test Front CA"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		apiFrontPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	})
	return apiFrontPEM
}

// enrolFixture wires a handler over a mock whose tenant already has a Ready
// instance and one active user.
func enrolFixture(t *testing.T) (*TenantTAKHandler, *mockStore, *fakeTAKCerts) {
	t.Helper()
	m := &mockStore{
		takInstance: &store.TAKInstance{
			TenantID: "test-tenant", Label: "abcdefghij",
			State: "Running", Phase: "Ready",
		},
		takUser: &store.TAKUser{TenantID: "test-tenant", Username: "phone01", Active: true},
	}
	certs := &fakeTAKCerts{fx: newTAKCertFixture(t, "phone01"), outcome: TAKCertReady}
	h := takHandlerOn(t, m)
	h.SetTAKCerts(certs, apiFrontTrust(t))
	return h, m, certs
}

func enrolRouter(h *TenantTAKHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/tenant/tak/users/{username}/enrollment", h.Enrol)
	r.Get("/api/tak/enroll/{claimID}/{nonce}", h.Claim)
	return r
}

func mint(t *testing.T, h *TenantTAKHandler) enrolmentResponse {
	t.Helper()
	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users/phone01/enrollment", "")
	enrolRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var out enrolmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("mint: decode: %v", err)
	}
	return out
}

func claim(t *testing.T, h *TenantTAKHandler, url string) *httptest.ResponseRecorder {
	t.Helper()
	// Deliberately NO tenant in the context: this endpoint is unauthenticated and
	// must work for a caller who has no account at all.
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	enrolRouter(h).ServeHTTP(rec, req)
	return rec
}

// TestTheStashHoldsNoUsableSecret is the point of the design: the private key is
// encrypted under the nonce, and the nonce is never stored. A database dump or a
// backup holds a blob nobody can open.
func TestTheStashHoldsNoUsableSecret(t *testing.T) {
	h, m, _ := enrolFixture(t)
	out := mint(t, h)

	var stashed string
	for k, v := range m.sysConfig {
		if strings.HasPrefix(k, "tak_enroll:") {
			stashed = v
		}
	}
	if stashed == "" {
		t.Fatal("nothing was stashed")
	}
	for _, leak := range []string{"PRIVATE KEY", "BEGIN RSA"} {
		if strings.Contains(stashed, leak) {
			t.Errorf("the stash contains %q in plaintext:\n%s", leak, stashed)
		}
	}
	// The nonce is in the URL the customer gets and must NOT be in the database.
	nonce := out.URL[strings.LastIndex(out.URL, "/")+1:]
	if nonce == "" || strings.Contains(stashed, nonce) {
		t.Error("the nonce is stored alongside the ciphertext, which defeats the point")
	}
	if m.updatedTAKUser == nil || m.updatedTAKUser.EnrollTokenHash == "" {
		t.Fatal("no enrolment token hash was recorded")
	}
	if m.updatedTAKUser.EnrollTokenHash == nonce {
		t.Error("the RAW token was stored; it must be a hash")
	}
	sum := sha256.Sum256([]byte(nonce))
	if m.updatedTAKUser.EnrollTokenHash != hex.EncodeToString(sum[:]) {
		t.Error("the stored hash is not SHA-256 of the token")
	}
	if m.updatedTAKUser.EnrollExpiresAt == nil {
		t.Error("no expiry was recorded, so the UI cannot say when it lapses")
	}
}

func TestTheSealedKeyNeedsTheNonce(t *testing.T) {
	sealed, err := sealKey([]byte("-----BEGIN RSA PRIVATE KEY-----\nsecret\n-----END RSA PRIVATE KEY-----\n"),
		"00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openKey(sealed, "00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100"); err != nil {
		t.Fatalf("the right nonce did not open it: %v", err)
	}
	if _, err := openKey(sealed, "ffffffffffffffffffffffffffffffff", "ffeeddccbbaa99887766554433221100"); err == nil {
		t.Error("a wrong nonce opened the key")
	}
	// A different claim id must not open it either, or one leaked nonce would
	// unlock every stash it was ever used with.
	if _, err := openKey(sealed, "00112233445566778899aabbccddeeff", "00000000000000000000000000000000"); err == nil {
		t.Error("the ciphertext is not bound to its claim id")
	}
}

func TestClaimingDeliversThePackageExactlyOnce(t *testing.T) {
	h, m, certs := enrolFixture(t)
	out := mint(t, h)

	rec := claim(t, h, out.URL)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "phone01_CONFIG.zip") {
		t.Errorf("content disposition = %q", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("a credential was served with Cache-Control %q", cc)
	}
	// It must be a real zip a client could open.
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err != nil {
		t.Errorf("the delivered package is not a zip: %v", err)
	}
	// The issued certificate is recorded, so the fleet page can show it and the
	// Authorizer can refuse a revoked serial later.
	if m.updatedTAKUser == nil || m.updatedTAKUser.CertSerial != "0A0B0C" {
		t.Errorf("the certificate serial was not recorded: %+v", m.updatedTAKUser)
	}
	if m.updatedTAKUser.EnrollTokenHash != "" {
		t.Error("the enrolment token survived the claim")
	}
	if len(certs.discarded) == 0 {
		t.Error("the certificate request was not discarded after collection")
	}

	// Second attempt: the same URL must now be refused, with the uniform string.
	again := claim(t, h, out.URL)
	if again.Code != http.StatusNotFound {
		t.Errorf("second claim: status = %d, want 404", again.Code)
	}
	if !strings.Contains(again.Body.String(), enrolClaimRefused) {
		t.Errorf("second claim said %q, want the uniform refusal", again.Body.String())
	}
}

func TestITAKVariantIsServedWhenAsked(t *testing.T) {
	h, _, _ := enrolFixture(t)
	out := mint(t, h)
	rec := claim(t, h, out.URL+"?client=itak")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "_CONFIG_iTAK.zip") {
		t.Errorf("content disposition = %q, want the iTAK package", cd)
	}
}

// TestAWrongNonceDoesNotBurnTheEnrolment: a guess must be refused without
// consuming the customer's one-time link, or anyone who can reach the endpoint
// can deny every enrolment by guessing once per claim id.
func TestAWrongNonceDoesNotBurnTheEnrolment(t *testing.T) {
	h, _, _ := enrolFixture(t)
	out := mint(t, h)
	parts := strings.Split(out.URL, "/")
	claimID := parts[len(parts)-2]

	bad := claim(t, h, "/api/tak/enroll/"+claimID+"/"+strings.Repeat("a", 32))
	if bad.Code != http.StatusNotFound || !strings.Contains(bad.Body.String(), enrolClaimRefused) {
		t.Fatalf("wrong nonce: status %d body %q", bad.Code, bad.Body.String())
	}
	// And the real one still works.
	good := claim(t, h, out.URL)
	if good.Code != http.StatusOK {
		t.Errorf("the genuine claim was burned by a wrong guess: status %d", good.Code)
	}
}

// TestAnUnknownClaimIsIndistinguishableFromAWrongNonce -- the two must not be
// separable, or the endpoint enumerates which enrolments exist.
func TestAnUnknownClaimIsIndistinguishableFromAWrongNonce(t *testing.T) {
	h, _, _ := enrolFixture(t)
	out := mint(t, h)
	parts := strings.Split(out.URL, "/")
	claimID := parts[len(parts)-2]

	unknown := claim(t, h, "/api/tak/enroll/"+strings.Repeat("b", 32)+"/"+strings.Repeat("c", 32))
	wrongNonce := claim(t, h, "/api/tak/enroll/"+claimID+"/"+strings.Repeat("c", 32))
	if unknown.Code != wrongNonce.Code || unknown.Body.String() != wrongNonce.Body.String() {
		t.Errorf("the two cases are distinguishable:\n  unknown: %d %s\n  wrong nonce: %d %s",
			unknown.Code, unknown.Body.String(), wrongNonce.Code, wrongNonce.Body.String())
	}
	// Malformed input is refused the same way rather than with a different error.
	malformed := claim(t, h, "/api/tak/enroll/short/alsoshort")
	if malformed.Code != unknown.Code {
		t.Errorf("a malformed path answered %d, want %d", malformed.Code, unknown.Code)
	}
}

func TestAnExpiredEnrolmentIsGoneAndCleared(t *testing.T) {
	h, m, _ := enrolFixture(t)
	out := mint(t, h)
	parts := strings.Split(out.URL, "/")
	claimID, nonce := parts[len(parts)-2], parts[len(parts)-1]

	// Age the stash by rewriting it with an old timestamp, which is the only part
	// of this the handler reads for expiry.
	var stash enrolStash
	if err := json.Unmarshal([]byte(m.sysConfig[enrolStashKey(claimID)]), &stash); err != nil {
		t.Fatal(err)
	}
	stash.CreatedAt = time.Now().Add(-enrolTTL - time.Minute)
	blob, _ := json.Marshal(stash)
	m.sysConfig[enrolStashKey(claimID)] = string(blob)

	rec := claim(t, h, "/api/tak/enroll/"+claimID+"/"+nonce)
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", rec.Code)
	}
	if m.sysConfig[enrolStashKey(claimID)] != "" {
		t.Error("an expired stash was left in place")
	}
}

func TestPendingIssuanceAnswers202AndKeepsTheEnrolment(t *testing.T) {
	h, m, certs := enrolFixture(t)
	out := mint(t, h)
	certs.outcome = TAKCertPending

	rec := claim(t, h, out.URL)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("202 with no Retry-After; a phone has nothing to go on")
	}
	parts := strings.Split(out.URL, "/")
	if m.sysConfig[enrolStashKey(parts[len(parts)-2])] == "" {
		t.Fatal("a pending claim consumed the enrolment; the retry would fail forever")
	}
	// And once signed, the same link works.
	certs.outcome = TAKCertReady
	if rec2 := claim(t, h, out.URL); rec2.Code != http.StatusOK {
		t.Errorf("after issuance the claim returned %d", rec2.Code)
	}
}

func TestARefusedCertificateIsTerminal(t *testing.T) {
	h, m, certs := enrolFixture(t)
	out := mint(t, h)
	certs.outcome = TAKCertRefused
	certs.msg = "username must be 3 to 32 lowercase characters"

	rec := claim(t, h, out.URL)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "username must be") {
		t.Errorf("the reason was not passed on: %s", rec.Body.String())
	}
	parts := strings.Split(out.URL, "/")
	if m.sysConfig[enrolStashKey(parts[len(parts)-2])] != "" {
		t.Error("a terminal refusal left the stash in place")
	}
}

// TestInfrastructureFailureIsNotMistakenForPending: a sick cluster must answer
// 503, not 202, or a phone retries forever against a fault nobody is told about.
func TestInfrastructureFailureIsNotMistakenForPending(t *testing.T) {
	h, _, certs := enrolFixture(t)
	out := mint(t, h)
	certs.collectErr = errors.New("the API server is unreachable")
	certs.outcome = TAKCertPending

	rec := claim(t, h, out.URL)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestEnrolRefusesWhatCannotWork(t *testing.T) {
	ready := &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij", State: "Running", Phase: "Ready"}
	active := &store.TAKUser{TenantID: "test-tenant", Username: "phone01", Active: true}

	cases := []struct {
		name     string
		store    *mockStore
		attach   bool
		username string
		want     int
	}{
		{"no TAK server", &mockStore{takUser: active}, true, "phone01", http.StatusNotFound},
		{"server still building", &mockStore{
			takInstance: &store.TAKInstance{Label: "abcdefghij", Phase: "Provisioning"},
			takUser:     active,
		}, true, "phone01", http.StatusConflict},
		{"no such user", &mockStore{takInstance: ready}, true, "phone01", http.StatusNotFound},
		{"suspended user", &mockStore{takInstance: ready, takUser: &store.TAKUser{
			TenantID: "test-tenant", Username: "phone01", Active: false,
		}}, true, "phone01", http.StatusConflict},
		{"hyphenated username", &mockStore{takInstance: ready, takUser: active}, true,
			"phone-01", http.StatusBadRequest},
		{"hosted TAK not configured", &mockStore{takInstance: ready, takUser: active}, false,
			"phone01", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := takHandlerOn(t, tc.store)
			if tc.attach {
				h.SetTAKCerts(&fakeTAKCerts{fx: newTAKCertFixture(t, "phone01"), outcome: TAKCertReady},
					apiFrontTrust(t))
			}
			req, rec := takRequest(http.MethodPost,
				"/api/tenant/tak/users/"+tc.username+"/enrollment", "")
			enrolRouter(h).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestEnrolmentIsRefusedWithoutTheFrontsChain: a handler that can mint
// certificates but does not know what the front presents would build a package
// with an empty truststore. The phone would fail the TLS handshake, at the
// client, where nothing on our side records it -- so refuse up front instead.
func TestEnrolmentIsRefusedWithoutTheFrontsChain(t *testing.T) {
	h, _, certs := enrolFixture(t)
	h.SetTAKCerts(certs, nil)

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users/phone01/enrollment", "")
	enrolRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if len(certs.requested) != 0 {
		t.Error("a certificate was requested for a package that could not have worked")
	}
}

// TestTheCertificateIsAskedForUnderTheInstanceLabel: asking under the tenant id
// would put the customer's identity into an object name in a shared namespace.
func TestTheCertificateIsAskedForUnderTheInstanceLabel(t *testing.T) {
	h, _, certs := enrolFixture(t)
	mint(t, h)
	if len(certs.requested) != 1 || certs.requested[0] != "abcdefghij/phone01" {
		t.Errorf("requested = %v, want [abcdefghij/phone01]", certs.requested)
	}
	for _, r := range certs.requested {
		if strings.Contains(r, "test-tenant") {
			t.Error("the request carries the tenant id, which the label exists to avoid")
		}
	}
}

// The two package flavours (MESHSAT-1084). ATAK and WinTAK read a zip with a
// manifest; iTAK reads a flat one. They are different archive shapes, so handing
// an iTAK user the ATAK package does not "mostly work" -- it does not import. And
// because a claim is single use, finding that out on the phone costs the
// enrolment.
//
// Claim has always served both and selected on ?client=itak. What was missing was
// any way to ASK: every link and QR the TAK page produced was the ATAK one.
func TestAClaimURLCanAskForTheITAKPackage(t *testing.T) {
	const base = "https://hub.meshsat.net/api/tak/enroll/abc/def"

	if got := claimURLFor(base, "itak"); got != base+"?client=itak" {
		t.Errorf("itak URL = %q", got)
	}
	// Case and stray spacing come from a query string a person may have typed.
	for _, spelling := range []string{"iTAK", "ITAK", " itak ", "iTak"} {
		if got := claimURLFor(base, spelling); got != base+"?client=itak" {
			t.Errorf("claimURLFor(%q) = %q, want the iTAK package", spelling, got)
		}
	}

	// Anything else is ATAK, including nothing and a misspelling. This runs AFTER
	// the enrolment is minted, so refusing an unknown value would spend a claim and
	// hand back an error -- the default has to be the flavour most phones want.
	for _, spelling := range []string{"", "atak", "ATAK", "android", "winrak", "itakk"} {
		if got := claimURLFor(base, spelling); got != base {
			t.Errorf("claimURLFor(%q) = %q, want the plain ATAK URL", spelling, got)
		}
	}

	// A base that already carries a query keeps it. Nothing builds one today;
	// appending a second "?" would silently produce a URL the router never matches.
	withQuery := base + "?x=1"
	if got := claimURLFor(withQuery, "itak"); got != withQuery+"&client=itak" {
		t.Errorf("claimURLFor on a URL with a query = %q", got)
	}
}

// The minted response carries both, so the shape of a claim URL is decided in one
// place rather than rebuilt in JavaScript.
func TestTheMintedEnrolmentOffersBothPackages(t *testing.T) {
	out := &enrolmentResponse{}
	claim := "https://hub.meshsat.net/api/tak/enroll/abc/def"
	out.URL, out.ITAKURL = claim, claimURLFor(claim, "itak")

	if out.ITAKURL == out.URL {
		t.Fatal("both URLs are identical, so the iTAK package is unreachable")
	}
	if !strings.HasPrefix(out.ITAKURL, out.URL) {
		t.Errorf("the iTAK URL is not the same claim: %q vs %q", out.ITAKURL, out.URL)
	}
}
