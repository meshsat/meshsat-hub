package tak

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFederationPKI writes a CA and a leaf certificate signed by it into dir
// and returns the paths of the leaf certificate, its key and the CA.
func writeFederationPKI(t *testing.T, dir string) (certFile, keyFile, caFile string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "federation test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}

	write := func(name, block string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: block, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("hub.crt", "CERTIFICATE", leafDER),
		write("hub.key", "EC PRIVATE KEY", leafKeyDER),
		write("ca.crt", "CERTIFICATE", caDER)
}

func TestBuildTLSConfigRequiresCertKeyAndCA(t *testing.T) {
	certFile, keyFile, caFile := writeFederationPKI(t, t.TempDir())

	tests := []struct {
		name        string
		cfg         FederationConfig
		wantMissing []string
	}{
		{"nothing set", FederationConfig{},
			[]string{"HUB_TAK_FEDERATION_CERT", "HUB_TAK_FEDERATION_KEY", "HUB_TAK_FEDERATION_CA"}},
		// The dangerous case: a certificate but no CA would verify peers
		// against the system roots.
		{"no CA", FederationConfig{CertFile: certFile, KeyFile: keyFile},
			[]string{"HUB_TAK_FEDERATION_CA"}},
		{"no key", FederationConfig{CertFile: certFile, CAFile: caFile},
			[]string{"HUB_TAK_FEDERATION_KEY"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFederation(tt.cfg, nil).buildTLSConfig()
			if err == nil {
				t.Fatal("expected an error, got a TLS config")
			}
			for _, name := range tt.wantMissing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			}
		})
	}
}

func TestBuildTLSConfigPinsPeersToTheConfiguredCA(t *testing.T) {
	certFile, keyFile, caFile := writeFederationPKI(t, t.TempDir())

	tlsCfg, err := NewFederation(FederationConfig{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}, nil).buildTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil || tlsCfg.RootCAs == nil {
		t.Error("ClientCAs and RootCAs must both be the configured CA, never nil (nil means the system roots)")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1", len(tlsCfg.Certificates))
	}
}

func TestBuildTLSConfigRejectsACAFileWithNoCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeFederationPKI(t, dir)
	bogus := filepath.Join(dir, "bogus.crt")
	if err := os.WriteFile(bogus, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewFederation(FederationConfig{CertFile: certFile, KeyFile: keyFile, CAFile: bogus}, nil).buildTLSConfig()
	if err == nil || !strings.Contains(err.Error(), "invalid CA") {
		t.Fatalf("err = %v, want invalid CA", err)
	}
}

func TestFailedStartLeavesFederationStopped(t *testing.T) {
	f := NewFederation(FederationConfig{Enabled: true}, nil)
	if err := f.Start(context.Background()); err == nil {
		f.Stop()
		t.Fatal("Start succeeded with no TLS material")
	}
	if f.running.Load() {
		t.Error("running is still true after a failed Start")
	}
	if f.listener != nil {
		t.Error("a listener was opened by a failed Start")
	}
	// Stop after a failed Start must be safe: main's leader-lost path may call it.
	f.Stop()
}
