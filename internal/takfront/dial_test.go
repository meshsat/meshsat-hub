package takfront

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The upstream TLS policy is what stands between a tenant's CoT and anything that
// answers on the address the Hub dials. These tests drive DialUpstream against a
// real TLS listener; before MESHSAT-1466 the same policy was exercised only through
// the public front's tests, which went with the front.

// clientOnly issues a certificate that cannot serve TLS.
func clientOnly() certOpt {
	return func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
}

// upstream is a stand-in TAK server: it presents serverCert and demands a client
// certificate from hubCA, and reports the common name it was shown.
func upstream(t *testing.T, serverCert tls.Certificate, hubCA *testCA) (addr string, seen <-chan string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    hubCA.pool(),
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				tc := c.(*tls.Conn)
				_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tc.Handshake(); err != nil {
					return
				}
				if peers := tc.ConnectionState().PeerCertificates; len(peers) > 0 {
					ch <- peers[0].Subject.CommonName
				}
				_, _ = tc.Write([]byte("ok"))
			}(c)
		}
	}()
	return ln.Addr().String(), ch
}

func dialTenant(t *testing.T, addr string, hubCA, upstreamCA *testCA, serverName string) *Tenant {
	t.Helper()
	return &Tenant{
		TenantID:           "t-dial",
		Label:              "dial",
		Upstream:           addr,
		Identity:           hubCA.issue(t, "meshsat-hub-t-dial"),
		UpstreamCAs:        upstreamCA.pool(),
		UpstreamServerName: serverName,
	}
}

func dialAndRead(t *testing.T, tn *Tenant) error {
	t.Helper()
	conn, err := DialUpstream(context.Background(), tn, 3*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2)
	_, err = conn.Read(buf)
	return err
}

func TestTheHubReachesItsTenantsServerAsTheTenantsIdentity(t *testing.T) {
	hubCA, upCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA")
	// No names at all, as OpenTAKServer's own certificate is for an in-cluster
	// address: the chain is what is checked.
	addr, seen := upstream(t, upCA.issue(t, "opentakserver"), hubCA)

	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "")); err != nil {
		t.Fatalf("a server whose chain verifies against the tenant's CA was refused: %v", err)
	}
	select {
	case cn := <-seen:
		if cn != "meshsat-hub-t-dial" {
			t.Fatalf("the server was shown %q, want the tenant's Hub identity", cn)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server never saw a client certificate")
	}
}

func TestAServerFromAnotherCAIsRefusedEvenWithoutANameCheck(t *testing.T) {
	hubCA, upCA, otherCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA"), newTestCA(t, "other CA")
	addr, _ := upstream(t, otherCA.issue(t, "impostor", forDNS("tak.test")), hubCA)

	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "")); err == nil {
		t.Fatal("a server certificate from a CA the tenant does not trust was accepted (name check skipped)")
	}
	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "tak.test")); err == nil {
		t.Fatal("a server certificate from a CA the tenant does not trust was accepted (name checked)")
	}
}

func TestANamedUpstreamMustCarryThatName(t *testing.T) {
	hubCA, upCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA")
	addr, _ := upstream(t, upCA.issue(t, "tak", forDNS("tak.example.test")), hubCA)

	// Positive control first: the right name is accepted, so the refusal below
	// is the name and not a broken listener.
	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "tak.example.test")); err != nil {
		t.Fatalf("the matching name was refused: %v", err)
	}
	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "other.example.test")); err == nil {
		t.Fatal("a server certificate for another name was accepted")
	}
}

func TestACertificateThatCannotServeIsRefused(t *testing.T) {
	hubCA, upCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA")
	addr, _ := upstream(t, upCA.issue(t, "client-only", clientOnly()), hubCA)

	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "")); err == nil {
		t.Fatal("a certificate without server authentication was accepted as a TAK server")
	}
}

func TestAnExpiredServerCertificateIsRefused(t *testing.T) {
	hubCA, upCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA")
	addr, _ := upstream(t, upCA.issue(t, "stale", expired()), hubCA)

	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "")); err == nil {
		t.Fatal("an expired server certificate was accepted")
	}
}

// A tenant with no upstream CA pool must not be dialled at all: crypto/x509 would
// otherwise verify against the system roots, and any publicly trusted certificate
// at the address would pass.
//
// To make that fallback observable, the upstream's CA is put in the SYSTEM store
// for this process (SSL_CERT_FILE, read once, on first use: nothing in this
// package touches the system roots before this test). Without the guard in
// dialUpstreamWith the dial below would then succeed.
func TestATenantWithoutAnUpstreamCAIsNeverDialled(t *testing.T) {
	hubCA, upCA := newTestCA(t, "hub CA"), newTestCA(t, "upstream CA")
	sys := filepath.Join(t.TempDir(), "system-roots.pem")
	if err := os.WriteFile(sys, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upCA.cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", sys)
	t.Setenv("SSL_CERT_DIR", t.TempDir())
	addr, _ := upstream(t, upCA.issue(t, "opentakserver", forDNS("tak.example.test")), hubCA)

	// Positive control: the same server with the pool is reached.
	if err := dialAndRead(t, dialTenant(t, addr, hubCA, upCA, "")); err != nil {
		t.Fatalf("control: the server was refused with its CA: %v", err)
	}
	for _, name := range []string{"", "tak.example.test"} {
		tn := dialTenant(t, addr, hubCA, upCA, name)
		tn.UpstreamCAs = nil
		if err := dialAndRead(t, tn); err == nil {
			t.Errorf("a tenant with no upstream CA was dialled (server name %q)", name)
		}
	}
	if err := verifyChainOnly([]*x509.Certificate{upCA.cert}, nil, time.Now()); err == nil {
		t.Error("verifyChainOnly accepted a chain with no roots to check it against")
	}
}

func TestDialUpstreamNeedsATenant(t *testing.T) {
	if _, err := DialUpstream(context.Background(), nil, time.Second); err == nil {
		t.Fatal("a nil tenant was dialled")
	}
}
