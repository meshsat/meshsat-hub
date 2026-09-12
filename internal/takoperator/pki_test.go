package takoperator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// csrFor builds a PKCS#10 request whose subject says whatever the caller wants,
// which is the point: the issuer must ignore it.
func csrFor(t *testing.T, subject pkix.Name, key any) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:         subject,
		DNSNames:        []string{"attacker.example"},
		ExtraExtensions: nil,
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate RSA %d: %v", bits, err)
	}
	return k
}

func testCA(t *testing.T) *CA {
	t.Helper()
	certPEM, keyPEM, err := NewTenantCA("abcdefghij")
	if err != nil {
		t.Fatalf("NewTenantCA: %v", err)
	}
	ca, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	return ca
}

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("issued certificate is not PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	return c
}

// The reason this whole API exists. Upstream OpenTAKServer signs whatever common
// name enrollment hands it, so a user there can mint a certificate naming
// `administrator`. Here the CSR's subject contributes nothing.
func TestTheCSRSubjectIsIgnoredAndTheUsernameWins(t *testing.T) {
	ca := testCA(t)
	key := rsaKey(t, 2048)

	for _, asked := range []pkix.Name{
		{CommonName: "administrator"},
		{CommonName: "meshsatproxy", Organization: []string{"Somebody Else"}},
		{CommonName: ""},
	} {
		issued, err := ca.IssueFromCSR(csrFor(t, asked, key), "alice", EUDValidDays)
		if err != nil {
			t.Fatalf("issue for %q: %v", asked.CommonName, err)
		}
		leaf := parseLeaf(t, issued.CertPEM)
		if leaf.Subject.CommonName != "alice" {
			t.Errorf("CSR asked for %q, certificate says %q, want alice",
				asked.CommonName, leaf.Subject.CommonName)
		}
		// The CSR's SANs must not survive either: a phone certificate with
		// attacker-chosen DNS names is a server certificate waiting to happen.
		if len(leaf.DNSNames) != 0 {
			t.Errorf("CSR's DNS names leaked into the certificate: %v", leaf.DNSNames)
		}
		if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			t.Errorf("extended key usage = %v, want client auth only", leaf.ExtKeyUsage)
		}
	}
}

// A certificate must only ever be issued to somebody who holds the private key.
func TestACSRThatIsNotSignedByItsOwnKeyIsRefused(t *testing.T) {
	ca := testCA(t)
	good := csrFor(t, pkix.Name{CommonName: "alice"}, rsaKey(t, 2048))

	block, _ := pem.Decode(good)
	tampered := append([]byte(nil), block.Bytes...)
	// The signature is the tail of the DER, so flipping its last byte leaves a
	// parseable request whose signature no longer verifies.
	tampered[len(tampered)-1] ^= 0xff
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered})

	if _, err := ca.IssueFromCSR(bad, "alice", EUDValidDays); !errors.Is(err, ErrBadCSR) {
		t.Fatalf("err = %v, want ErrBadCSR", err)
	}
	if _, err := ca.IssueFromCSR([]byte("not pem at all"), "alice", EUDValidDays); !errors.Is(err, ErrBadCSR) {
		t.Fatalf("garbage: err = %v, want ErrBadCSR", err)
	}
}

func TestWeakKeysAreRefusedAndSoundOnesAccepted(t *testing.T) {
	ca := testCA(t)

	t.Run("RSA 1024 refused", func(t *testing.T) {
		csr := csrFor(t, pkix.Name{CommonName: "alice"}, rsaKey(t, 1024))
		if _, err := ca.IssueFromCSR(csr, "alice", EUDValidDays); !errors.Is(err, ErrWeakKey) {
			t.Fatalf("err = %v, want ErrWeakKey", err)
		}
	})

	t.Run("RSA 2048 accepted", func(t *testing.T) {
		csr := csrFor(t, pkix.Name{CommonName: "alice"}, rsaKey(t, 2048))
		if _, err := ca.IssueFromCSR(csr, "alice", EUDValidDays); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("ECDSA P-256 accepted", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		csr := csrFor(t, pkix.Name{CommonName: "alice"}, key)
		if _, err := ca.IssueFromCSR(csr, "alice", EUDValidDays); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
}

func TestAnIssuedCertificateVerifiesAgainstItsCAAndNotAnother(t *testing.T) {
	ca, other := testCA(t), testCA(t)
	csr := csrFor(t, pkix.Name{CommonName: "ignored"}, rsaKey(t, 2048))
	issued, err := ca.IssueFromCSR(csr, "alice", EUDValidDays)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	leaf := parseLeaf(t, issued.CertPEM)

	mine := x509.NewCertPool()
	if !mine.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not load")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: mine, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("should verify against its own CA: %v", err)
	}

	theirs := x509.NewCertPool()
	theirs.AppendCertsFromPEM(other.CertPEM())
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: theirs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Fatal("a certificate verified against a different tenant's CA")
	}
}

// A leaf outliving its issuer is a certificate that stops working for a reason
// nobody will connect to the CA.
func TestALeafNeverOutlivesItsCA(t *testing.T) {
	ca := testCA(t)
	csr := csrFor(t, pkix.Name{CommonName: "alice"}, rsaKey(t, 2048))
	issued, err := ca.IssueFromCSR(csr, "alice", 100*365)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	leaf := parseLeaf(t, issued.CertPEM)
	if leaf.NotAfter.After(ca.cert.NotAfter) {
		t.Errorf("leaf expires %s, after the CA's %s", leaf.NotAfter, ca.cert.NotAfter)
	}
}

func TestPurposeLifetimes(t *testing.T) {
	ca := testCA(t)
	csr := csrFor(t, pkix.Name{CommonName: "x"}, rsaKey(t, 2048))
	for _, tc := range []struct{ days, wantDays int }{
		{EUDValidDays, 90},
		{HubValidDays, 7},
		{0, 90}, // zero means the phone default, never "forever"
	} {
		issued, err := ca.IssueFromCSR(csr, "alice", tc.days)
		if err != nil {
			t.Fatalf("issue %d days: %v", tc.days, err)
		}
		got := int(time.Until(issued.NotAfter).Hours() / 24)
		if got < tc.wantDays-1 || got > tc.wantDays {
			t.Errorf("validDays %d gave %d days, want about %d", tc.days, got, tc.wantDays)
		}
	}
}

func TestServerCertificateIsForServerAuth(t *testing.T) {
	ca := testCA(t)
	certPEM, keyPEM, err := ca.IssueServerCert("tak-abcdefghij.meshsat-tak.svc",
		[]string{"tak-abcdefghij.meshsat-tak.svc", "tak-abcdefghij"}, 0)
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}
	leaf := parseLeaf(t, certPEM)
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage = %v, want server auth", leaf.ExtKeyUsage)
	}
	if len(leaf.DNSNames) != 2 {
		t.Errorf("DNS names = %v, want both service names", leaf.DNSNames)
	}
	if !strings.Contains(string(keyPEM), "RSA PRIVATE KEY") {
		t.Error("server key is not a PEM RSA key")
	}
}

func TestLoadCARefusesSomethingThatIsNotACA(t *testing.T) {
	ca := testCA(t)
	// A leaf, not a CA.
	csr := csrFor(t, pkix.Name{CommonName: "alice"}, rsaKey(t, 2048))
	issued, err := ca.IssueFromCSR(csr, "alice", EUDValidDays)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, keyPEM, err := NewTenantCA("klmnopqrst")
	if err != nil {
		t.Fatalf("NewTenantCA: %v", err)
	}
	if _, err := LoadCA(issued.CertPEM, keyPEM); err == nil {
		t.Fatal("LoadCA accepted a leaf certificate as a CA")
	}
	if _, err := LoadCA([]byte("nope"), keyPEM); err == nil {
		t.Fatal("LoadCA accepted garbage")
	}
}

// The CA subject must not say who the customer is: it travels to every phone and
// is visible in any handshake.
func TestTheCASubjectLeaksOnlyTheOpaqueLabel(t *testing.T) {
	certPEM, _, err := NewTenantCA("abcdefghij")
	if err != nil {
		t.Fatalf("NewTenantCA: %v", err)
	}
	subject := parseLeaf(t, certPEM).Subject.String()
	if !strings.Contains(subject, "abcdefghij") {
		t.Errorf("subject %q should carry the label", subject)
	}
	for _, leak := range []string{"tenant", "slug", "@"} {
		if strings.Contains(strings.ToLower(subject), leak) {
			t.Errorf("subject %q looks like it carries %q", subject, leak)
		}
	}
}

func TestWrappingTheCAKey(t *testing.T) {
	key, err := ParseWrapKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseWrapKey: %v", err)
	}
	_, caKeyPEM, err := NewTenantCA("abcdefghij")
	if err != nil {
		t.Fatalf("NewTenantCA: %v", err)
	}

	sealed, err := WrapKeyMaterial(key, caKeyPEM)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if strings.Contains(string(sealed), "PRIVATE KEY") {
		t.Fatal("the wrapped blob still contains the plaintext key")
	}
	back, err := UnwrapKeyMaterial(key, sealed)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if string(back) != string(caKeyPEM) {
		t.Fatal("round trip did not return the original key")
	}

	// Wrapping twice must not produce the same bytes, or a repeated ciphertext
	// would tell an observer that two tenants share a CA key.
	again, err := WrapKeyMaterial(key, caKeyPEM)
	if err != nil {
		t.Fatalf("wrap again: %v", err)
	}
	if string(again) == string(sealed) {
		t.Fatal("wrapping is deterministic; the nonce is not random")
	}

	t.Run("a wrong wrap key fails closed", func(t *testing.T) {
		other, err := ParseWrapKey(strings.Repeat("cd", 32))
		if err != nil {
			t.Fatalf("ParseWrapKey: %v", err)
		}
		if _, err := UnwrapKeyMaterial(other, sealed); err == nil {
			t.Fatal("unwrapped with the wrong key")
		}
	})

	t.Run("a truncated blob fails closed", func(t *testing.T) {
		if _, err := UnwrapKeyMaterial(key, sealed[:8]); err == nil {
			t.Fatal("unwrapped a blob too short to hold a nonce")
		}
	})

	t.Run("a flipped bit fails closed", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(bad)-1] ^= 0x01
		if _, err := UnwrapKeyMaterial(key, bad); err == nil {
			t.Fatal("GCM accepted a tampered ciphertext")
		}
	})
}

func TestParseWrapKeyRefusesTheWrongShape(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"not hex":   strings.Repeat("zz", 32),
		"too short": strings.Repeat("ab", 16),
		"too long":  strings.Repeat("ab", 48),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWrapKey(in); !errors.Is(err, ErrWrapKey) {
				t.Fatalf("err = %v, want ErrWrapKey", err)
			}
		})
	}
}

// Names derived from the label must be stable and must not collide, because they
// address a database whose contents are a customer's history.
func TestDerivedNames(t *testing.T) {
	if got := DatabaseName("abcdefghij"); got != "tak_abcdefghij" {
		t.Errorf("DatabaseName = %q", got)
	}
	if InstanceName("abcdefghij") == InstanceName("klmnopqrst") {
		t.Error("two labels produced the same instance name")
	}
	// The role name and the database name must be EQUAL: the cluster's pg_hba is
	// `host sameuser all all scram-sha-256` followed by `host all all all
	// reject`, so a role may reach only the database whose name matches its own.
	// If these ever diverge, tenant isolation silently stops working.
	//
	// Asserted through the objects that actually carry the two names, rather
	// than by comparing DatabaseName to itself — which is what this test did
	// until staticcheck pointed out it could never fail.
	const label = "abcdefghij"
	db := DatabaseObject(label, "meshsat-tak-main", false, false)
	role := RoleObject(label, "meshsat-tak-main", false)
	if db.Spec.Name != role.Spec.Name {
		t.Errorf("database is %q but the role is %q; `host sameuser` needs them equal",
			db.Spec.Name, role.Spec.Name)
	}
	if db.Spec.Owner != role.Spec.Name {
		t.Errorf("database owner is %q but the role is %q", db.Spec.Owner, role.Spec.Name)
	}
	// And the teardown default must be retain, so losing an object cannot lose a
	// customer's history.
	if db.Spec.ReclaimPolicy != ReclaimRetain || role.Spec.ReclaimPolicy != ReclaimRetain {
		t.Errorf("reclaim policies are %q/%q, want both %q",
			db.Spec.ReclaimPolicy, role.Spec.ReclaimPolicy, ReclaimRetain)
	}
	if purged := DatabaseObject(label, "meshsat-tak-main", false, true); purged.Spec.ReclaimPolicy != ReclaimDelete {
		t.Errorf("a purge left the reclaim policy %q, want %q", purged.Spec.ReclaimPolicy, ReclaimDelete)
	}
}
