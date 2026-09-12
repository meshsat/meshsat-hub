package takhosted

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// A tenant's TAK server that refuses this Hub's certificate must not lose their
// positions in silence (MESHSAT-1066).
//
// Why this needed a new harness rather than reusing capturedOTS: that one is a
// plain TCP listener, and the defect only exists over TLS. Under TLS 1.3 the
// client certificate is sent after the server's Finished, so the dial succeeds,
// the first write lands in the kernel buffer and returns nil, and the rejection
// is never noticed. Proven on production before this test existed: four refusals
// at the customer's server produced zero warnings at the Hub and four identical
// "opened a CoT connection" lines.
//
// The assertions below fail if the forward is reported as successful, which is
// exactly what the shipped code did.

// --- a certificate authority that can mint leaves, which takhosted's existing
// --- testCA cannot: it only signs CSRs.

type mtlsCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newMTLSCA(t *testing.T, cn string) *mtlsCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca self-sign: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &mtlsCA{cert: cert, key: key}
}

func (ca *mtlsCA) leaf(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth,
		},
	}
	if server {
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue %s: %v", cn, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse %s: %v", cn, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}
}

func (ca *mtlsCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// --- a tenant's TAK server that demands a client certificate ----------------

type mtlsOTS struct {
	addr string

	mu    sync.Mutex
	lines []string
}

// newMTLSOTS starts a TLS listener requiring a client certificate signed by
// trusts. It reads and discards and never writes, which is what a real
// OpenTAKServer does on this socket -- and is what makes silence the healthy
// signal the probe relies on.
func newMTLSOTS(t *testing.T, own, trusts *mtlsCA) *mtlsOTS {
	t.Helper()
	s := &mtlsOTS{}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{own.leaf(t, "tenant-tak-server", true)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    trusts.pool(),
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("mtls listen: %v", err)
	}
	s.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						s.mu.Lock()
						s.lines = append(s.lines, string(buf[:n]))
						s.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return s
}

func (s *mtlsOTS) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.lines...)
}

// forwarderWithCert wires a Forwarder that presents clientCert to ots, dialling
// with the same TLS shape dialUpstreamWith builds: the chain verified against the
// tenant's CA with the name check skipped.
func forwarderWithCert(
	t *testing.T, ots *mtlsOTS, serverCA *mtlsCA, clientCert tls.Certificate, label string,
) (*Forwarder, *fakeBus, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tenant := &takfront.Tenant{TenantID: "t1", Label: label, Upstream: ots.addr}
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 5 * time.Second},
			Config: &tls.Config{
				Certificates: []tls.Certificate{clientCert},
				RootCAs:      serverCA.pool(),
				MinVersion:   tls.VersionTLS12,
			},
		}
		return d.DialContext(ctx, "tcp", tn.Upstream)
	}
	ups := func(_ context.Context, id string) []*takfront.Tenant {
		if id == "t1" {
			return []*takfront.Tenant{tenant}
		}
		return nil
	}
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "300434", "FIELD-KIT-1", "rockblock"), dial, ups, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	return f, b, logs
}

// counterValue reads one label pair of the failure counter out of the default
// registry. Gathering directly rather than using prometheus/testutil, which would
// add a module requirement for a test-only convenience.
func counterValue(t *testing.T, kind, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "meshsat_hub_takhosted_forward_failed_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotKind, gotReason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "kind":
					gotKind = l.GetValue()
				case "reason":
					gotReason = l.GetValue()
				}
			}
			if gotKind == kind && gotReason == reason {
				return m.GetCounter().GetValue()
			}
		}
		t.Fatalf("no series for kind=%q reason=%q; the init() materialisation is missing, "+
			"so a rate alert could never fire on it", kind, reason)
	}
	t.Fatal("the counter meshsat_hub_takhosted_forward_failed_total is not registered")
	return 0
}

// The headline case. A certificate the tenant's server does not trust must be
// reported, not swallowed.
func TestAServerThatRefusesOurCertificateIsReportedNotSwallowed(t *testing.T) {
	serverCA := newMTLSCA(t, "tenant server CA")
	trusted := newMTLSCA(t, "the CA the tenant's server trusts")
	rogue := newMTLSCA(t, "a CA the tenant's server has never heard of")

	ots := newMTLSOTS(t, serverCA, trusted)
	// Deliberately the SAME common name a working Hub presents. The issuer is what
	// must decide, not the name.
	_, b, logs := forwarderWithCert(t, ots, serverCA, rogue.leaf(t, "meshsat-hub", false), externalLabel)

	before := counterValue(t, kindExternal, reasonRefused)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}
	// Long enough for the probe window plus send's one retry.
	time.Sleep(2 * time.Second)

	out := logs.String()

	if !strings.Contains(out, "forwarding a position failed") {
		t.Errorf("the Hub did not report the failure at all.\nThis is the defect: the dial "+
			"succeeds, the first write returns nil, and the position is lost in silence.\nLog was:\n%s", out)
	}
	if strings.Contains(out, "opened a CoT connection") {
		t.Errorf("the Hub logged that it OPENED a connection the server had already refused, "+
			"which is what made a broken configuration look like a working one.\nLog was:\n%s", out)
	}
	if got := counterValue(t, kindExternal, reasonRefused); got <= before {
		t.Errorf("the refusal counter did not move (%v -> %v); without it an operator has only "+
			"logs to notice a customer losing every position", before, got)
	}
	if got := ots.received(); len(got) != 0 {
		t.Errorf("the server recorded %d payloads; it refused the certificate so it should have "+
			"received nothing: %q", len(got), got)
	}
	// And the reason is one somebody can act on, not an opaque wrapper.
	if !strings.Contains(out, "certificate") {
		t.Errorf("the failure does not mention a certificate, so the customer cannot tell what to "+
			"fix.\nLog was:\n%s", out)
	}
}

// The control, and it is not optional: without it the test above would pass on a
// forwarder that simply never works.
func TestAServerThatTrustsOurCertificateStillReceivesCoT(t *testing.T) {
	serverCA := newMTLSCA(t, "tenant server CA")
	trusted := newMTLSCA(t, "the CA the tenant's server trusts")

	ots := newMTLSOTS(t, serverCA, trusted)
	_, b, logs := forwarderWithCert(t, ots, serverCA, trusted.leaf(t, "meshsat-hub", false), externalLabel)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = ots.received(); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatalf("a trusted certificate delivered nothing, so the probe is refusing healthy "+
			"connections.\nLog was:\n%s", logs.String())
	}
	joined := strings.Join(got, "")
	for _, want := range []string{"<event", "meshsat-device-300434", "FIELD-KIT-1", "52.1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CoT is missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(logs.String(), "opened a CoT connection") {
		t.Errorf("a healthy connection did not log that it opened.\nLog was:\n%s", logs.String())
	}
}

// kindOf must not leak a hosted instance's per-tenant label into a metric.
func TestTheMetricKindIsDerivedAndNeverACustomerIdentifier(t *testing.T) {
	if got := kindOf(&takfront.Tenant{Label: externalLabel}); got != kindExternal {
		t.Errorf("kindOf(external) = %q, want %q", got, kindExternal)
	}
	// A hosted instance's label is ten random characters, one value per customer.
	for _, label := range []string{"abcdefghij", "zx9q2w8e7r", ""} {
		if got := kindOf(&takfront.Tenant{Label: label}); got != kindHosted {
			t.Errorf("kindOf(%q) = %q, want %q: a random per-tenant label must collapse to "+
				"one metric value, or the series count grows with the customer count", label, got, kindHosted)
		}
	}
	if got := kindOf(nil); got != kindHosted {
		t.Errorf("kindOf(nil) = %q, want a safe default", got)
	}
}

// The probe must treat an idle connection as healthy, or every tenant's first
// position would be refused.
func TestAnIdleConnectionIsHealthyNotRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Accept and say nothing, like a real TAK server.
		t.Cleanup(func() { _ = conn.Close() })
		time.Sleep(3 * time.Second)
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if why := upstreamRejected(c); why != nil {
		t.Errorf("an idle, healthy connection was rejected: %v", why)
	}
}

// And a peer that hangs up immediately must be caught.
func TestAPeerThatHangsUpIsRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if why := upstreamRejected(c); why == nil {
		t.Error("a peer that closed the connection immediately was treated as healthy, " +
			"so its tenant's positions would go nowhere in silence")
	}
}
