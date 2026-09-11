package takfront

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"
)

// tenantFor builds a minimally valid Tenant around a CA, so each test only has
// to state what it is actually about.
func tenantFor(t *testing.T, id string, ca *testCA) Tenant {
	t.Helper()
	upstreamCA := newTestCA(t, "ots-ca-"+id)
	return Tenant{
		TenantID:    id,
		Label:       "lbl" + id,
		CA:          ca.cert,
		Upstream:    "ots-" + id + ".invalid:8089",
		Identity:    upstreamCA.issue(t, "meshsatproxy"),
		UpstreamCAs: upstreamCA.pool(),
	}
}

// The issuer is the only thing in a TAK stream that names the tenant, because
// the client sends no SNI and no hostname. This is the whole premise.
func TestTheIssuerNamesTheTenant(t *testing.T) {
	caA, caB := newTestCA(t, "tenant-a-ca"), newTestCA(t, "tenant-b-ca")
	dir, err := NewDirectory([]Tenant{tenantFor(t, "aaa", caA), tenantFor(t, "bbb", caB)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	for _, tc := range []struct {
		ca   *testCA
		user string
		want string
	}{
		{caA, "alice", "aaa"},
		{caB, "bob", "bbb"},
		{caA, "bob", "aaa"}, // the user name does not influence the tenant
	} {
		peer, err := dir.Resolve(rawChain(tc.ca.issue(t, tc.user)), time.Now())
		if err != nil {
			t.Fatalf("resolve %s/%s: %v", tc.want, tc.user, err)
		}
		if peer.Tenant.TenantID != tc.want {
			t.Errorf("user %s resolved to tenant %q, want %q", tc.user, peer.Tenant.TenantID, tc.want)
		}
		if peer.CommonName != tc.user {
			t.Errorf("common name = %q, want %q", peer.CommonName, tc.user)
		}
	}
}

// A certificate from any other CA must not land in a tenant. This is what stops
// one customer's phone, or anybody's self-signed certificate, reaching another
// customer's server on the shared port.
func TestACertificateFromAnotherCAResolvesToNoTenant(t *testing.T) {
	caA := newTestCA(t, "tenant-a-ca")
	dir, err := NewDirectory([]Tenant{tenantFor(t, "aaa", caA)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	stranger := newTestCA(t, "somebody-else-ca")
	if _, err := dir.Resolve(rawChain(stranger.issue(t, "alice")), time.Now()); !errors.Is(err, ErrUnknownIssuer) {
		t.Fatalf("stranger CA: err = %v, want ErrUnknownIssuer", err)
	}

	// Same subject as the real CA, different key: the issuer name matches, so
	// the lookup succeeds and the signature check is what must refuse it.
	twin := newTestCA(t, "tenant-a-ca")
	_, err = dir.Resolve(rawChain(twin.issue(t, "alice")), time.Now())
	if err == nil || errors.Is(err, ErrUnknownIssuer) {
		t.Fatalf("CA with a copied subject: err = %v, want a verification failure", err)
	}
	if !strings.Contains(err.Error(), "aaa") {
		t.Errorf("error should name the tenant it was checked against, got %v", err)
	}
}

func TestResolveRefusesCertificatesItCannotAttribute(t *testing.T) {
	ca := newTestCA(t, "tenant-a-ca")
	dir, err := NewDirectory([]Tenant{tenantFor(t, "aaa", ca)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	t.Run("no certificate at all", func(t *testing.T) {
		if _, err := dir.Resolve(nil, time.Now()); !errors.Is(err, ErrNoCertificate) {
			t.Fatalf("err = %v, want ErrNoCertificate", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		if _, err := dir.Resolve(rawChain(ca.issue(t, "alice", expired())), time.Now()); err == nil {
			t.Fatal("an expired certificate resolved")
		}
	})

	t.Run("no common name", func(t *testing.T) {
		_, err := dir.Resolve(rawChain(ca.issue(t, "alice", noCommonName())), time.Now())
		if !errors.Is(err, ErrNoCommonName) {
			t.Fatalf("err = %v, want ErrNoCommonName", err)
		}
	})

	t.Run("not valid for client authentication", func(t *testing.T) {
		if _, err := dir.Resolve(rawChain(ca.issue(t, "alice", serverOnly())), time.Now()); err == nil {
			t.Fatal("a server-only certificate resolved")
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		if _, err := dir.Resolve([][]byte{[]byte("not a certificate")}, time.Now()); err == nil {
			t.Fatal("garbage resolved")
		}
	})
}

// An expired certificate is refused at the moment of the handshake, not at the
// moment the directory was built.
func TestResolveUsesTheTimePassedIn(t *testing.T) {
	ca := newTestCA(t, "tenant-a-ca")
	dir, err := NewDirectory([]Tenant{tenantFor(t, "aaa", ca)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	cert := ca.issue(t, "alice")
	if _, err := dir.Resolve(rawChain(cert), time.Now()); err != nil {
		t.Fatalf("valid now: %v", err)
	}
	if _, err := dir.Resolve(rawChain(cert), time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("still valid two hours after it expired")
	}
}

// Two tenants sharing a CA subject would make the issuer ambiguous, and the
// front would route a phone into whichever tenant happened to be indexed.
func TestTwoTenantsMayNotShareACASubject(t *testing.T) {
	ca := newTestCA(t, "shared-ca")
	_, err := NewDirectory([]Tenant{tenantFor(t, "aaa", ca), tenantFor(t, "bbb", ca)})
	if err == nil {
		t.Fatal("NewDirectory accepted two tenants with one CA subject")
	}
	for _, want := range []string{"aaa", "bbb", "share"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestNewDirectoryRefusesIncompleteTenants(t *testing.T) {
	ca := newTestCA(t, "tenant-a-ca")
	for name, breakIt := range map[string]func(*Tenant){
		"no id":       func(tn *Tenant) { tn.TenantID = "" },
		"no CA":       func(tn *Tenant) { tn.CA = nil },
		"no upstream": func(tn *Tenant) { tn.Upstream = "" },
		"no identity": func(tn *Tenant) { tn.Identity = tls.Certificate{} },
		"no upstream CAs": func(tn *Tenant) {
			tn.UpstreamCAs = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			tn := tenantFor(t, "aaa", ca)
			breakIt(&tn)
			if _, err := NewDirectory([]Tenant{tn}); err == nil {
				t.Fatalf("NewDirectory accepted a tenant with %s", name)
			}
		})
	}
}

// ClientCAs is only the hint sent to clients about acceptable issuers. It must
// cover every tenant, and must not be what verification relies on.
func TestClientCAsCoversEveryTenant(t *testing.T) {
	caA, caB := newTestCA(t, "tenant-a-ca"), newTestCA(t, "tenant-b-ca")
	dir, err := NewDirectory([]Tenant{tenantFor(t, "aaa", caA), tenantFor(t, "bbb", caB)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	if got := dir.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	subjects := dir.ClientCAs().Subjects() //nolint:staticcheck // the hint list is exactly what we assert
	if len(subjects) != 2 {
		t.Fatalf("ClientCAs has %d subjects, want 2", len(subjects))
	}
	// Verifying against the union would succeed for both tenants, which is why
	// Resolve uses one tenant's pool instead.
	union := x509.NewCertPool()
	union.AddCert(caA.cert)
	union.AddCert(caB.cert)
	for _, ca := range []*testCA{caA, caB} {
		cert := ca.issue(t, "alice")
		if _, err := cert.Leaf.Verify(x509.VerifyOptions{
			Roots:     union,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Fatalf("union pool should accept every tenant's phone: %v", err)
		}
	}
}
