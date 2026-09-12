package takfront

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The Authorizer double is server_test.go's allowAll: the two NewServer
// assertions at the bottom are about whether a certificate was supplied, not
// about authorisation, and a second stub for the same interface in the same
// package is just a thing to keep in step.

// The renewal case is the one worth testing, because it happens once every sixty
// days and fails for every tenant at once. Nothing else in this package would
// notice: the front would keep answering, the handshakes would fail at the phone,
// and our logs would show connections arriving and going away.

// writePair writes a self-signed certificate and key to the two paths and returns
// the certificate, so a test can assert WHICH one is being served.
func writePair(t *testing.T, certPath, keyPath, cn string) *x509.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func certFileFixture(t *testing.T) (*CertFile, string, string, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	first := writePair(t, certPath, keyPath, "first.meshsat.test")
	cf, err := NewCertFile(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewCertFile: %v", err)
	}
	// Check on every call, so a test does not have to sleep a minute.
	cf.checkEvery = 0
	return cf, certPath, keyPath, first
}

func served(t *testing.T, cf *CertFile) *x509.Certificate {
	t.Helper()
	got, err := cf.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf := leafOf(got)
	if leaf == nil {
		t.Fatal("the served certificate has no parsable leaf")
	}
	return leaf
}

func TestItServesTheCertificateOnDisk(t *testing.T) {
	cf, _, _, first := certFileFixture(t)
	if got := served(t, cf); !got.Equal(first) {
		t.Errorf("served %q, want %q", got.Subject.CommonName, first.Subject.CommonName)
	}
	if cf.NotAfter().IsZero() {
		t.Error("NotAfter is zero; an alert on expiry would have nothing to read")
	}
}

// TestARenewalIsPickedUpWithoutARestart is the whole reason this type exists.
func TestARenewalIsPickedUpWithoutARestart(t *testing.T) {
	cf, certPath, keyPath, first := certFileFixture(t)
	if got := served(t, cf); !got.Equal(first) {
		t.Fatal("wrong certificate before the renewal")
	}

	// What cert-manager's renewal looks like from in here: the files change.
	second := writePair(t, certPath, keyPath, "second.meshsat.test")

	got := served(t, cf)
	if !got.Equal(second) {
		t.Errorf("after a renewal the front still serves %q; every phone would fail "+
			"the moment the old certificate expired", got.Subject.CommonName)
	}
	if cf.Reloads() != 1 {
		t.Errorf("Reloads() = %d, want 1", cf.Reloads())
	}
}

// TestAHalfWrittenUpdateKeepsThePreviousCertificate: a volume update is not
// atomic from our side, so a read can land between the two files being written.
// Serving nothing would take the listener down for a condition that resolves in
// milliseconds.
func TestAHalfWrittenUpdateKeepsThePreviousCertificate(t *testing.T) {
	cf, certPath, _, first := certFileFixture(t)

	// A certificate that does not match the key on disk: exactly what a partial
	// write looks like.
	if err := os.WriteFile(certPath, []byte("-----BEGIN CERTIFICATE-----\nbroken\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := served(t, cf); !got.Equal(first) {
		t.Errorf("a broken update changed what is served, to %q", got.Subject.CommonName)
	}
	if cf.Reloads() != 0 {
		t.Errorf("Reloads() = %d; a failed load must not count", cf.Reloads())
	}

	// And when the write completes, it IS picked up -- the failure must not latch.
	second := writePair(t, certPath, filepath.Join(filepath.Dir(certPath), "tls.key"), "third.meshsat.test")
	if got := served(t, cf); !got.Equal(second) {
		t.Errorf("after the write completed the front still serves %q", got.Subject.CommonName)
	}
}

func TestAMissingFileKeepsThePreviousCertificate(t *testing.T) {
	cf, certPath, _, first := certFileFixture(t)
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if got := served(t, cf); !got.Equal(first) {
		t.Errorf("a missing file changed what is served")
	}
	if _, err := cf.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("GetCertificate returned an error with a usable certificate in hand: %v", err)
	}
}

// TestTheFilesAreNotStatedOnEveryHandshake: a phone reconnects constantly, and
// this is on the handshake path.
func TestTheFilesAreNotStatedOnEveryHandshake(t *testing.T) {
	cf, certPath, keyPath, first := certFileFixture(t)
	cf.checkEvery = time.Hour

	writePair(t, certPath, keyPath, "ignored.meshsat.test")
	if got := served(t, cf); !got.Equal(first) {
		t.Error("the interval was ignored; the files are being stat'ed every handshake")
	}

	// Moving the clock past the interval makes it notice, which proves the
	// interval is the only thing holding it back.
	base := time.Now()
	cf.now = func() time.Time { return base.Add(2 * time.Hour) }
	if got := served(t, cf); got.Equal(first) {
		t.Error("after the interval elapsed the change was still not picked up")
	}
}

func TestNewCertFileRefusesWhatItCannotLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewCertFile(filepath.Join(dir, "nope.crt"), filepath.Join(dir, "nope.key"), nil); err == nil {
		t.Error("a front was given a certificate path that does not exist")
	}
}

// A Server must accept the callback in place of a static certificate, and must
// still refuse to exist with neither.
func TestServerTakesEitherACertificateOrACallback(t *testing.T) {
	cf, _, _, _ := certFileFixture(t)
	if _, err := NewServer(Config{GetCertificate: cf.GetCertificate}, allowAll{}, nil); err != nil {
		t.Errorf("a front with a reloading certificate was refused: %v", err)
	}
	if _, err := NewServer(Config{}, allowAll{}, nil); err == nil {
		t.Error("a front with no certificate at all was accepted")
	}
}
