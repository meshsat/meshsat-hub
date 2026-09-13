package takhosted

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// IdentityKeeper had no tests, and that is how MESHSAT-1076 reached production:
// with two replicas sharing one request object, each deleted the other's issued
// certificate forever, no tenant entered the front's directory, and not one phone
// could connect. Every pipeline was green.
//
// These tests run REAL keepers against a fake API server that really signs the
// CSRs it is given, because the thing that breaks is `tls.X509KeyPair` refusing a
// certificate that belongs to another replica's key. A fake that handed back a
// canned certificate would pass either way and prove nothing.

// fakeCRAPI is enough of the Kubernetes API for certificate requests: apply, get,
// delete -- and it signs on apply, standing in for the operator.
type fakeCRAPI struct {
	mu      sync.Mutex
	objects map[string]*TakCertificateRequest
	caCert  *x509.Certificate
	caKey   *rsa.PrivateKey
	caPEM   string

	// applied and deleted record the order of operations per object name, which is
	// how a test sees one replica trampling another.
	applied []string
	deleted []string
	// denyAll makes every request come back Denied.
	denyAll bool
}

func newFakeCRAPI(t *testing.T) *fakeCRAPI {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake tenant CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeCRAPI{
		objects: map[string]*TakCertificateRequest{},
		caCert:  cert,
		caKey:   key,
		caPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// sign issues a certificate for the CSR's OWN public key, which is what makes a
// certificate usable only by the replica that asked.
func (f *fakeCRAPI) sign(t *testing.T, csrPEM, username string) (string, string) {
	t.Helper()
	blk, _ := pem.Decode([]byte(csrPEM))
	if blk == nil {
		t.Fatalf("the keeper sent something that is not a PEM CSR: %q", csrPEM[:min(40, len(csrPEM))])
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatalf("unparseable CSR: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		// The operator forces the common name; mirror that here.
		Subject:     pkix.Name{CommonName: username},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), f.caPEM
}

func (f *fakeCRAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		name := parts[len(parts)-1]

		f.mu.Lock()
		defer f.mu.Unlock()

		switch r.Method {
		case http.MethodPatch: // server-side apply is how the client creates
			var req TakCertificateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			f.applied = append(f.applied, name)
			if f.denyAll {
				req.Status = CertReqStat{Phase: CertDenied, Message: "refused by the fake operator"}
			} else {
				certPEM, caPEM := f.sign(t, req.Spec.CSRPEM, req.Spec.Username)
				req.Status = CertReqStat{
					Phase: CertIssued, CertPEM: certPEM, CACertPEM: caPEM,
					Serial: "00" + name,
				}
			}
			f.objects[name] = &req
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(req)

		case http.MethodGet:
			obj, ok := f.objects[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","code":404,"reason":"NotFound"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(obj)

		case http.MethodDelete:
			f.deleted = append(f.deleted, name)
			if _, ok := f.objects[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","code":404,"reason":"NotFound"}`))
				return
			}
			delete(f.objects, name)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kind":"Status","code":200}`))

		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// settle drives For() until it yields a certificate or gives up, because issuance
// is asynchronous by design: the first call asks and returns ErrIdentityPending.
func settle(t *testing.T, k *IdentityKeeper, tenant, label string) (string, error) {
	t.Helper()
	var lastErr error
	for i := 0; i < 4; i++ {
		pair, err := k.For(context.Background(), tenant, label)
		if err == nil {
			if pair.Leaf == nil && len(pair.Certificate) == 0 {
				return "", errors.New("a nil certificate came back with no error")
			}
			leaf, perr := x509.ParseCertificate(pair.Certificate[0])
			if perr != nil {
				return "", perr
			}
			return leaf.SerialNumber.String(), nil
		}
		lastErr = err
		if !errors.Is(err, ErrIdentityPending) {
			return "", err
		}
	}
	return "", lastErr
}

// TestTwoReplicasEachGetTheirOwnIdentity is the regression test for MESHSAT-1076.
// On the old code one replica ends up with no usable certificate, because the
// other deleted or replaced the object it was waiting on.
func TestTwoReplicasEachGetTheirOwnIdentity(t *testing.T) {
	api := newFakeCRAPI(t)
	cl, _ := newTestClient(t, api.handler(t))

	a := NewIdentityKeeper(cl, "hub-aaaa-1", nil)
	b := NewIdentityKeeper(cl, "hub-bbbb-2", nil)

	// Interleaved on purpose: this is what two replicas refreshing independently
	// actually looks like, and the interleaving is what broke.
	_, _ = a.For(context.Background(), "t1", "abcdefghij")
	_, _ = b.For(context.Background(), "t1", "abcdefghij")

	serialA, errA := settle(t, a, "t1", "abcdefghij")
	serialB, errB := settle(t, b, "t1", "abcdefghij")

	if errA != nil {
		t.Errorf("replica A never obtained an identity: %v", errA)
	}
	if errB != nil {
		t.Errorf("replica B never obtained an identity: %v", errB)
	}
	if errA != nil || errB != nil {
		t.Fatal("with a shared request object only one replica can ever succeed; " +
			"both must, or the front serves phones from one replica only")
	}
	if serialA == serialB {
		t.Error("both replicas hold the SAME certificate, so one is using a key it does not have")
	}

	// And they must have asked under different names -- the actual fix.
	names := map[string]bool{}
	for _, n := range api.applied {
		names[n] = true
	}
	if len(names) != 2 {
		t.Errorf("requests were applied under %d distinct names, want 2: %v", len(names), api.applied)
	}
	for n := range names {
		if !strings.HasPrefix(n, "hub-abcdefghij-") {
			t.Errorf("object name %q does not carry the tenant label and a replica suffix", n)
		}
	}
}

func TestTheRequestNameCarriesTheReplica(t *testing.T) {
	one := certRequestName("abcdefghij", "hub-aaaa-1")
	two := certRequestName("abcdefghij", "hub-bbbb-2")
	if one == two {
		t.Fatal("two replicas produce the same request name; they will fight over it")
	}
	// Deterministic, so a retry lands on the same object rather than queueing a
	// second signature.
	if certRequestName("abcdefghij", "hub-aaaa-1") != one {
		t.Error("the name is not deterministic for one replica")
	}
	// Valid as a Kubernetes object name, and short.
	for _, n := range []string{one, two} {
		if len(n) > 253 || strings.ContainsAny(n, "_ ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("%q is not a valid object name", n)
		}
	}
	// A different tenant is a different object even for the same replica.
	if certRequestName("klmnopqrst", "hub-aaaa-1") == one {
		t.Error("two tenants share one request name for the same replica")
	}
}

// An empty replica id must NOT collapse to a shared name: a missing POD_NAME
// would otherwise reintroduce the defect silently.
func TestAnEmptyReplicaIDDoesNotCollide(t *testing.T) {
	api := newFakeCRAPI(t)
	cl, _ := newTestClient(t, api.handler(t))
	a := NewIdentityKeeper(cl, "", nil)
	b := NewIdentityKeeper(cl, "", nil)
	if a.replica == "" || b.replica == "" {
		t.Fatal("an empty replica id was kept as empty")
	}
	if a.replica == b.replica {
		t.Fatal("two keepers with no replica id got the same one")
	}
}

// A replica that restarts keeps its pod name, finds a certificate it has no key
// for, and must recover by asking again -- the branch that was only ever SAFE
// once the object belonged to one replica.
func TestARestartedReplicaRecoversItsOwnIdentity(t *testing.T) {
	api := newFakeCRAPI(t)
	cl, _ := newTestClient(t, api.handler(t))

	first := NewIdentityKeeper(cl, "hub-same-1", nil)
	if _, err := first.For(context.Background(), "t1", "abcdefghij"); !errors.Is(err, ErrIdentityPending) {
		t.Fatalf("first ask: %v", err)
	}
	// The process restarts: same replica id, no in-memory key.
	restarted := NewIdentityKeeper(cl, "hub-same-1", nil)
	serial, err := settle(t, restarted, "t1", "abcdefghij")
	if err != nil {
		t.Fatalf("a restarted replica never recovered: %v", err)
	}
	if serial == "" {
		t.Error("no certificate after recovery")
	}
	if len(api.deleted) == 0 {
		t.Error("the stale request was not cleared before asking again")
	}
}

// This test used to be called "...AndNotRetriedForever" and asserted that a
// refusal was simply returned. That was the defect, not the design
// (MESHSAT-1075): the refused object stayed, every refresh read the same Denied
// status, and the tenant was absent from the front until a person deleted the
// object by hand. When the front was first switched on, EVERY tenant was refused
// by a username validator that a later merge corrected -- and the Hub would have
// stayed dark after the fix landed.
func TestARefusalIsRetriedAndCarriesTheReason(t *testing.T) {
	api := newFakeCRAPI(t)
	api.denyAll = true
	cl, _ := newTestClient(t, api.handler(t))

	k := NewIdentityKeeper(cl, "hub-aaaa-1", nil)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return now }

	_, _ = k.For(context.Background(), "t1", "abcdefghij")

	// The first refusal is acted on at once, because the usual cause is transient.
	_, err := k.For(context.Background(), "t1", "abcdefghij")
	if !errors.Is(err, ErrIdentityPending) {
		t.Fatalf("the first refusal gave %v, want it to have asked again (ErrIdentityPending)", err)
	}
	if len(api.deleted) == 0 {
		t.Fatal("the refused request was not cleared before asking again")
	}

	// The second says so, rather than deleting and recreating on every refresh.
	_, err = k.For(context.Background(), "t1", "abcdefghij")
	if !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a refusal inside the backoff gave %v, want ErrIdentityDenied", err)
	}
	if !strings.Contains(err.Error(), "refused by the fake operator") {
		t.Errorf("the operator's reason was dropped: %v", err)
	}
}

// The whole point of the backoff: a genuinely bad request must not mean one delete
// and one create per tenant on every refresh for ever. The refresh runs every five
// minutes against every tenant.
func TestAPersistentRefusalBacksOffInsteadOfChurning(t *testing.T) {
	api := newFakeCRAPI(t)
	api.denyAll = true
	cl, _ := newTestClient(t, api.handler(t))

	k := NewIdentityKeeper(cl, "hub-aaaa-1", nil)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return now }

	_, _ = k.For(context.Background(), "t1", "abcdefghij") // creates
	_, _ = k.For(context.Background(), "t1", "abcdefghij") // refused -> retried
	afterFirstRetry := len(api.deleted)

	// Thirty more refreshes INSIDE the window touch the API server not at all.
	// Three quarters of the window, deliberately short of the boundary: walking
	// exactly to it makes the last refresh legitimately due, which is a flaky test
	// rather than a defect.
	for i := 0; i < 30; i++ {
		now = now.Add(denialRetryBase / 40)
		if _, err := k.For(context.Background(), "t1", "abcdefghij"); !errors.Is(err, ErrIdentityDenied) {
			t.Fatalf("refresh %d gave %v, want ErrIdentityDenied", i, err)
		}
	}
	if len(api.deleted) != afterFirstRetry {
		t.Errorf("deleted %d times during the backoff, want %d: it is churning the API server",
			len(api.deleted), afterFirstRetry)
	}

	// Once the window has passed it tries again, so a cause fixed later is picked up.
	now = now.Add(denialRetryBase + time.Minute)
	if _, err := k.For(context.Background(), "t1", "abcdefghij"); !errors.Is(err, ErrIdentityPending) {
		t.Fatalf("after the backoff gave %v, want another attempt", err)
	}
	if len(api.deleted) <= afterFirstRetry {
		t.Error("the backoff never expired, so the tenant is dark for good")
	}
}

// A cause that is fixed between refreshes must be picked up, which is the scenario
// that actually happened on production.
func TestARefusalThatStopsIsForgotten(t *testing.T) {
	api := newFakeCRAPI(t)
	api.denyAll = true
	cl, _ := newTestClient(t, api.handler(t))

	k := NewIdentityKeeper(cl, "hub-aaaa-1", nil)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return now }

	_, _ = k.For(context.Background(), "t1", "abcdefghij")
	_, _ = k.For(context.Background(), "t1", "abcdefghij") // refused, asked again

	// The operator is fixed -- on production this was a merge that corrected the
	// username validator. The Hub waits out the current backoff and then recovers
	// BY ITSELF, which is the entire point: before this, it never did.
	//
	// In practice recovery is usually immediate rather than delayed, because a fix
	// that ships as a new image rolls the pods and a fresh keeper remembers no
	// refusals. The backoff only bites a CRD-only change with no Hub rollout.
	api.denyAll = false
	now = now.Add(denialRetryBase + time.Minute)
	if _, err := settle(t, k, "t1", "abcdefghij"); err != nil {
		t.Fatalf("a tenant never recovered after the cause was fixed: %v", err)
	}

	// And the refusal is forgotten, so a later one starts from an immediate retry
	// rather than serving out a backoff earned long ago.
	k.mu.Lock()
	_, remembered := k.refused["t1"]
	k.mu.Unlock()
	if remembered {
		t.Error("a tenant that recovered still carries its old refusal")
	}
}

func TestTheRefusalBackoffLadder(t *testing.T) {
	for _, tc := range []struct {
		tries int
		want  time.Duration
	}{
		{0, 0}, {1, 0}, // the first attempt is immediate, deliberately
		{2, denialRetryBase},
		{3, 2 * denialRetryBase},
		{4, 4 * denialRetryBase},
		{5, 8 * denialRetryBase},
		{6, denialRetryMax}, {7, denialRetryMax}, {99, denialRetryMax},
	} {
		if got := retryAfter(tc.tries); got != tc.want {
			t.Errorf("retryAfter(%d) = %s, want %s", tc.tries, got, tc.want)
		}
	}
	if denialRetryMax < denialRetryBase {
		t.Error("the cap is below the base, so the ladder would go backwards")
	}
}

// Forget is what the purge path calls. A tenant that is removed and later
// recreated must not inherit a backoff earned by a previous incarnation.
func TestForgetClearsARefusal(t *testing.T) {
	api := newFakeCRAPI(t)
	api.denyAll = true
	cl, _ := newTestClient(t, api.handler(t))
	k := NewIdentityKeeper(cl, "hub-aaaa-1", nil)

	_, _ = k.For(context.Background(), "t1", "abcdefghij")
	_, _ = k.For(context.Background(), "t1", "abcdefghij")
	k.Forget("t1")

	k.mu.Lock()
	_, remembered := k.refused["t1"]
	k.mu.Unlock()
	if remembered {
		t.Error("Forget left the refusal behind")
	}
}

// Holding reports what the readiness probe reads, so it must reflect reality.
func TestHoldingReportsWhatIsActuallyHeld(t *testing.T) {
	api := newFakeCRAPI(t)
	cl, _ := newTestClient(t, api.handler(t))
	k := NewIdentityKeeper(cl, "hub-aaaa-1", nil)

	if len(k.Holding()) != 0 {
		t.Error("something is held before anything was asked for")
	}
	if _, err := settle(t, k, "t1", "abcdefghij"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	held := k.Holding()
	if len(held) != 1 {
		t.Fatalf("Holding() = %d tenants, want 1", len(held))
	}
	if exp, ok := held["t1"]; !ok || exp.Before(time.Now()) {
		t.Errorf("held expiry for t1 is %v, want a future time", exp)
	}
	k.Forget("t1")
	if len(k.Holding()) != 0 {
		t.Error("Forget did not drop the identity")
	}
}

// The name the Hub asks for lives in THREE places: this package, the operator,
// and a CEL literal in the CRD. The API server enforces the CRD's, so that is the
// arbiter -- and the import that would collapse the duplication is forbidden by
// this package's security boundary (see boundary_test.go), so a test is what keeps
// them in step.
//
// This is not hypothetical. MESHSAT-1088 changed the operator and the CRD and
// missed this copy; every hub certificate request came back
//
//	422 ... username must be 3 to 32 lowercase alphanumerics for an eud
//	certificate, or exactly meshsat_hub for a hub certificate
//
// the directory emptied to `tenants:0 skipped:1` on both replicas, and the front
// refused every phone with SSLV3_ALERT_BAD_CERTIFICATE -- because with no tenant
// in the directory there is no issuer to attribute a certificate to. Every
// pipeline was green.
func TestTheHubAsksForTheNameTheCRDAdmits(t *testing.T) {
	raw, err := os.ReadFile("../../k8s/tak-operator/crds/takcertificaterequest.yaml")
	if err != nil {
		t.Skipf("CRD not readable from here: %v", err)
	}
	want := "self.username == '" + HubIdentity + "'"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("the CRD does not admit %q for a hub certificate.\n"+
			"The API server refuses what this package sends, so the Hub can obtain no "+
			"upstream identity, the directory empties, and NO phone in ANY tenant can "+
			"connect.\nLooked for: %s", HubIdentity, want)
	}
	// And the reason the separator matters at all: OpenTAKServer's own validator
	// allows letters, numbers, underscore and full stop -- never a hyphen.
	if strings.Contains(HubIdentity, "-") {
		t.Errorf("HubIdentity = %q uses a hyphen, which OpenTAKServer's username "+
			"validator refuses, so no account can exist for it and the tenant's "+
			"server will silently store nothing", HubIdentity)
	}
}
