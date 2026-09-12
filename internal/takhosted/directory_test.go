package takhosted

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// --- a tiny CA, so tests can mint the PEM the operator would publish ---------

type testCA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pemStr string
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"MeshSat"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &testCA{
		cert: cert, key: key,
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// signCSR issues a client certificate for a CSR, the way the operator does.
func (ca *testCA) signCSR(t *testing.T, csrPEM string, cn string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("CSR is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		// The operator DISCARDS the CSR subject and issues the username it was
		// told; mirroring that here keeps the test honest about who decides.
		Subject:     pkix.Name{CommonName: cn, Organization: ca.cert.Subject.Organization},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// --- a fake API server holding instances and certificate requests ------------

type fakeAPI struct {
	mu        sync.Mutex
	instances []TakInstance
	reqs      map[string]*TakCertificateRequest
	ca        *testCA
	// autoIssue mimics the operator: a created request is signed immediately.
	autoIssue bool
	listErr   bool
	creates   int
	deletes   int
	// instOps records per-INSTANCE operations in the order they arrive, because
	// the teardown's load-bearing property is a sequence: the purge marker must be
	// patched on BEFORE the delete. Counting both would pass just as well if they
	// happened the wrong way round, which retains the customer's database.
	instOps []string
	// instPatchErr makes a patch fail, to prove the teardown then refuses to
	// delete an unmarked instance.
	instPatchErr bool
}

func newFakeAPI(ca *testCA) *fakeAPI {
	return &fakeAPI{reqs: map[string]*TakCertificateRequest{}, ca: ca}
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/takinstances"):
			if f.listErr {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"api is unwell"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(instanceList{Items: f.instances})

		case strings.Contains(r.URL.Path, "/takcertificaterequests/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			switch r.Method {
			case http.MethodGet:
				req, ok := f.reqs[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"kind":"Status","message":"not found"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(req)
			case http.MethodPatch:
				var in TakCertificateRequest
				_ = json.NewDecoder(r.Body).Decode(&in)
				f.creates++
				if f.autoIssue {
					in.Status = CertReqStat{
						Phase:   CertIssued,
						CertPEM: f.ca.signCSR(t, in.Spec.CSRPEM, in.Spec.Username),
						Serial:  "4242",
					}
				} else {
					in.Status = CertReqStat{Phase: "Pending"}
				}
				cp := in
				f.reqs[name] = &cp
				_ = json.NewEncoder(w).Encode(cp)
			case http.MethodDelete:
				f.deletes++
				delete(f.reqs, name)
				_, _ = w.Write([]byte(`{}`))
			}

		// One instance by name. Without this case a PATCH or DELETE of an instance
		// fell to the default 404, which the client reads as ErrNotFound -- so a
		// teardown test would have passed while tearing nothing down.
		case strings.Contains(r.URL.Path, "/takinstances/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			switch r.Method {
			case http.MethodPatch:
				var in TakInstance
				_ = json.NewDecoder(r.Body).Decode(&in)
				f.instOps = append(f.instOps,
					"patch:"+name+":"+PurgeAnnotation+"="+in.Metadata.Annotations[PurgeAnnotation])
				if f.instPatchErr {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"kind":"Status","message":"patch refused"}`))
					return
				}
				for i := range f.instances {
					if f.instances[i].Metadata.Name != name {
						continue
					}
					if f.instances[i].Metadata.Annotations == nil {
						f.instances[i].Metadata.Annotations = map[string]string{}
					}
					for k, v := range in.Metadata.Annotations {
						f.instances[i].Metadata.Annotations[k] = v
					}
					_ = json.NewEncoder(w).Encode(f.instances[i])
					return
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"not found"}`))
			case http.MethodDelete:
				f.instOps = append(f.instOps, "delete:"+name)
				var kept []TakInstance
				for _, inst := range f.instances {
					if inst.Metadata.Name != name {
						kept = append(kept, inst)
					}
				}
				f.instances = kept
				f.deletes++
				_, _ = w.Write([]byte(`{}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"unexpected method"}`))
			}

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"unexpected path"}`))
		}
	})
}

// ops returns the per-instance operations seen, in order.
func (f *fakeAPI) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.instOps...)
}

// instanceNames returns the instances still present.
func (f *fakeAPI) instanceNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, inst := range f.instances {
		out = append(out, inst.Metadata.Name)
	}
	return out
}

func (f *fakeAPI) setInstances(in ...TakInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances = in
}

func readyInstance(tenantID, label, caPEM string) TakInstance {
	return TakInstance{
		Metadata: objectMeta{Name: "tak-" + label},
		Spec:     InstanceSpec{TenantID: tenantID, Label: label, State: "Running"},
		Status: InstanceStat{
			Phase: PhaseReady, Host: "tak-" + label + ".meshsat-tak.svc:8089",
			CACertPEM: caPEM,
		},
	}
}

// prime runs one refresh purely to ASK for the Hub's upstream certificates.
//
// Issuance is deliberately two-pass: ask() always returns ErrIdentityPending,
// because in production the operator signs on its own ticker and a refresh that
// blocked waiting for a signature would stall every other tenant. So the first
// pass requests and the second collects, and a test that refreshes once can never
// see a tenant served -- which is exactly how three tests here first "passed"
// while asserting nothing.
func prime(t *testing.T, r *DirectoryRefresher) {
	t.Helper()
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("priming refresh: %v", err)
	}
}

// PublishEmpty is not a nicety. takfront.Server.Serve dereferences the stored
// directory on its first line, and SetDirectory(nil) is a silent no-op, so a
// front started with no snapshot nil-panics the moment it listens. A cluster with
// no TAK tenants is the ordinary starting state, not an edge case.
func TestAnEmptyDirectoryIsPublishableSoTheFrontCanStartWithNoTenants(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")

	var got *takfront.Directory
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(d *takfront.Directory) { got = d }, nil)

	if err := r.PublishEmpty(); err != nil {
		t.Fatalf("PublishEmpty: %v", err)
	}
	if got == nil {
		t.Fatal("nothing was published; Serve would nil-panic on its first line")
	}
	if got.Len() != 0 {
		t.Errorf("empty directory has %d tenants", got.Len())
	}
}

// The batch behaviour that matters most.
//
// takfront.NewDirectory is all-or-nothing: it errors on the FIRST tenant missing
// an id, CA, upstream or identity. So one tenant still provisioning must not take
// the others down with it -- it is skipped, with a reason, and the healthy tenants
// are served.
func TestAProvisioningTenantIsSkippedRatherThanFailingTheWholeBatch(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = true

	f.setInstances(
		readyInstance("t-ready", "aaaaaaaaaa", ca.pemStr),
		// Not Ready: the operator is still building it.
		TakInstance{
			Metadata: objectMeta{Name: "tak-bbbbbbbbbb"},
			Spec:     InstanceSpec{TenantID: "t-building", Label: "bbbbbbbbbb"},
			Status:   InstanceStat{Phase: "Provisioning"},
		},
		// Ready but reports no CA yet.
		TakInstance{
			Metadata: objectMeta{Name: "tak-cccccccccc"},
			Spec:     InstanceSpec{TenantID: "t-noca", Label: "cccccccccc"},
			Status:   InstanceStat{Phase: PhaseReady, Host: "h:8089"},
		},
		// Ready but its CA is nonsense: not transient, and must not poison the batch.
		TakInstance{
			Metadata: objectMeta{Name: "tak-dddddddddd"},
			Spec:     InstanceSpec{TenantID: "t-badca", Label: "dddddddddd"},
			Status: InstanceStat{
				Phase: PhaseReady, Host: "h:8089",
				CACertPEM: "-----BEGIN CERTIFICATE-----\nnot base64 at all\n-----END CERTIFICATE-----\n",
			},
		},
	)

	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	var got *takfront.Directory
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(d *takfront.Directory) { got = d }, nil)

	// First pass asks for the Ready tenant's certificate; second serves it.
	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned an error instead of skipping the unready tenants: %v", err)
	}
	if got == nil {
		t.Fatal("nothing published")
	}
	if got.Len() != 1 {
		t.Errorf("directory serves %d tenants, want just the Ready one", got.Len())
	}
	n, skipped := r.Snapshot()
	if n != 1 {
		t.Errorf("snapshot reports %d tenants, want 1", n)
	}
	for _, want := range []string{"t-building", "t-noca", "t-badca"} {
		why, ok := skipped[want]
		if !ok {
			t.Errorf("%s was not recorded as skipped, so nobody can find out why it is absent", want)
			continue
		}
		if why == "" {
			t.Errorf("%s was skipped with no reason given", want)
		}
	}
}

// A refresh that fails must keep serving the snapshot it has. Replacing a working
// directory with nothing because the API server blinked would drop every phone in
// every tenant -- far worse than a few minutes of slightly stale tenant set.
func TestAFailedRefreshKeepsThePreviousSnapshot(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = true
	f.setInstances(readyInstance("t-ready", "aaaaaaaaaa", ca.pemStr))

	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")

	published := []*takfront.Directory{}
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(d *takfront.Directory) { published = append(published, d) }, nil)

	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if len(published) == 0 || published[len(published)-1].Len() != 1 {
		t.Fatalf("the working snapshot was not published: %d publishes, last has %d tenants",
			len(published), published[len(published)-1].Len())
	}
	// Everything from here is measured against the state AFTER a good refresh.
	base := len(published)

	// Now the API server breaks.
	f.mu.Lock()
	f.listErr = true
	f.mu.Unlock()

	if err := r.Refresh(context.Background()); err == nil {
		t.Error("a failing API server produced no error")
	}
	if len(published) != base {
		t.Errorf("a failed refresh published a new snapshot (%d, was %d); the previous one must stand",
			len(published), base)
	}
	if n, _ := r.Snapshot(); n != 1 {
		t.Errorf("after a failed refresh the snapshot reports %d tenants, want the previous 1", n)
	}
}

// The Hub's upstream identity is asked for, not self-signed, and the wait does not
// block the refresh: the first pass skips the tenant, the second serves it.
func TestTheHubAsksForItsUpstreamIdentityAndIsSkippedUntilItArrives(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	f := newFakeAPI(ca)
	f.autoIssue = false // the operator has not signed yet
	f.setInstances(readyInstance("t-one", "aaaaaaaaaa", ca.pemStr))

	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	var got *takfront.Directory
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(d *takfront.Directory) { got = d }, nil)

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh while pending: %v", err)
	}
	if got.Len() != 0 {
		t.Errorf("a tenant with no issued identity was served: %d tenants", got.Len())
	}
	if _, skipped := r.Snapshot(); !strings.Contains(skipped["t-one"], "certificate") {
		t.Errorf("skip reason %q does not mention the certificate", skipped["t-one"])
	}
	f.mu.Lock()
	created := f.creates
	f.mu.Unlock()
	if created == 0 {
		t.Error("no certificate request was created; the Hub must ASK rather than self-sign")
	}

	// The operator signs it, and the next refresh serves the tenant.
	f.mu.Lock()
	for name, req := range f.reqs {
		req.Status = CertReqStat{
			Phase:   CertIssued,
			CertPEM: ca.signCSR(t, req.Spec.CSRPEM, req.Spec.Username),
			Serial:  "4242",
		}
		f.reqs[name] = req
	}
	f.mu.Unlock()

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after issuance: %v", err)
	}
	if got.Len() != 1 {
		t.Fatalf("after issuance the directory serves %d tenants, want 1", got.Len())
	}
}

// Two tenants sharing a CA subject is a real defect, not a skip: the issuer is the
// only thing naming a tenant, so an ambiguous one would route a phone into
// somebody else's instance. The previous snapshot must stand.
func TestTwoTenantsSharingACASubjectFailsLoudlyAndKeepsTheOldSnapshot(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK shared CA")
	f := newFakeAPI(ca)
	f.autoIssue = true
	f.setInstances(
		readyInstance("t-one", "aaaaaaaaaa", ca.pemStr),
		readyInstance("t-two", "bbbbbbbbbb", ca.pemStr), // same CA: ambiguous
	)

	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	published := 0
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(*takfront.Directory) { published++ }, nil)

	// Prime so BOTH tenants have their identities, or they would both be skipped
	// and NewDirectory would be handed an empty list -- which is how this test
	// first passed while proving nothing about duplicate subjects.
	prime(t, r)
	before := published
	err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("a duplicate CA subject was accepted; a phone could be routed into the wrong tenant")
	}
	if published != before {
		t.Errorf("a broken directory was published (%d publishes, was %d)", published, before)
	}
	if !strings.Contains(err.Error(), "CA subject") {
		t.Errorf("error %q does not name the cause", err)
	}
}
