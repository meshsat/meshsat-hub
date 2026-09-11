package takfront

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeOTS stands in for a tenant's OpenTAKServer EUD listener: it demands a
// client certificate signed by that tenant's OTS CA, records which common name
// it saw, and relays whatever arrives to its other connections, which is what
// OpenTAKServer does with group CoT.
type fakeOTS struct {
	addr string
	ca   *testCA

	mu       sync.Mutex
	seen     []string
	received []byte
	conns    []net.Conn
}

func newFakeOTS(t *testing.T, name string) *fakeOTS {
	t.Helper()
	ca := newTestCA(t, "ots-ca-"+name)
	srv := &fakeOTS{ca: ca}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{ca.issue(t, "ots-"+name, forDNS("ots.test"))},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool(),
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("fake ots listen: %v", err)
	}
	srv.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(conn)
		}
	}()
	return srv
}

func (f *fakeOTS) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if err := tc.Handshake(); err != nil {
		return
	}
	cn := ""
	if certs := tc.ConnectionState().PeerCertificates; len(certs) > 0 {
		cn = certs[0].Subject.CommonName
	}
	f.mu.Lock()
	f.seen = append(f.seen, cn)
	f.conns = append(f.conns, conn)
	f.mu.Unlock()

	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			f.mu.Lock()
			f.received = append(f.received, chunk...)
			peers := make([]net.Conn, 0, len(f.conns))
			for _, c := range f.conns {
				if c != conn {
					peers = append(peers, c)
				}
			}
			f.mu.Unlock()
			for _, c := range peers {
				_, _ = c.Write(chunk)
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeOTS) snapshot() ([]string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...), append([]byte(nil), f.received...)
}

// allowAll is the Authorizer a tenant in good standing gets.
type allowAll struct{}

func (allowAll) AllowTAKClient(context.Context, string, string, *big.Int) error { return nil }

type denyFunc func(tenantID, cn string) error

func (d denyFunc) AllowTAKClient(_ context.Context, tenantID, cn string, _ *big.Int) error {
	return d(tenantID, cn)
}

// recordingRecorder captures what the server reports, so tests can assert the
// reason a connection was refused rather than inferring it from a log line.
type recordingRecorder struct {
	mu      sync.Mutex
	refused []string
	opened  []string
	closed  int
}

func (r *recordingRecorder) Refused(_ context.Context, tenantID, reason string, _ net.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = append(r.refused, tenantID+"/"+reason)
}

func (r *recordingRecorder) Opened(_ context.Context, p *Peer, _ net.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = append(r.opened, p.Tenant.TenantID+"/"+p.CommonName)
}

func (r *recordingRecorder) Closed(context.Context, *Peer, int64, int64, time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
}

func (r *recordingRecorder) state() ([]string, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refused...), append([]string(nil), r.opened...), r.closed
}

// front is a running server plus what a test needs to talk to it.
type front struct {
	addr       string
	platformCA *testCA
	srv        *Server
	rec        *recordingRecorder
}

// tenantOn wires a tenant whose phones are signed by phoneCA to a fake server.
func tenantOn(t *testing.T, id string, phoneCA *testCA, ots *fakeOTS) Tenant {
	t.Helper()
	return Tenant{
		TenantID:    id,
		Label:       "lbl" + id,
		CA:          phoneCA.cert,
		Upstream:    ots.addr,
		Identity:    ots.ca.issue(t, "meshsatproxy"),
		UpstreamCAs: ots.ca.pool(),
	}
}

func startFront(t *testing.T, cfg Config, authz Authorizer, tenants []Tenant) *front {
	t.Helper()
	platformCA := newTestCA(t, "meshsat platform CA")
	// One server certificate for every tenant: without SNI the front must
	// present it before it knows who is calling.
	cfg.Certificate = platformCA.issue(t, "tak.meshsat.net", forDNS("tak.test"))
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	rec := &recordingRecorder{}
	srv, err := NewServer(cfg, authz, rec)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	dir, err := NewDirectory(tenants)
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	srv.SetDirectory(dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, ln); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return &front{addr: ln.Addr().String(), platformCA: platformCA, srv: srv, rec: rec}
}

// dial connects as a phone would: it verifies the front's chain against the
// truststore it was shipped, and sends its own certificate.
func (f *front) dial(t *testing.T, cert tls.Certificate) (*tls.Conn, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", f.addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      f.platformCA.pool(),
		ServerName:   "tak.test",
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The point of the whole package: two customers on ONE port, each reaching only
// their own server, told apart by nothing but who signed their certificate.
func TestEachTenantsPhonesReachOnlyTheirOwnServer(t *testing.T) {
	otsA, otsB := newFakeOTS(t, "a"), newFakeOTS(t, "b")
	caA, caB := newTestCA(t, "tenant-a-ca"), newTestCA(t, "tenant-b-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{
		tenantOn(t, "aaa", caA, otsA),
		tenantOn(t, "bbb", caB, otsB),
	})

	alice, err := f.dial(t, caA.issue(t, "alice"))
	if err != nil {
		t.Fatalf("alice dial: %v", err)
	}
	bob, err := f.dial(t, caB.issue(t, "bob"))
	if err != nil {
		t.Fatalf("bob dial: %v", err)
	}
	if _, err := alice.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("alice write: %v", err)
	}
	if _, err := bob.Write([]byte("<event uid=\"bob\"/>\n")); err != nil {
		t.Fatalf("bob write: %v", err)
	}

	waitFor(t, "both servers to receive their own phone", func() bool {
		_, ra := otsA.snapshot()
		_, rb := otsB.snapshot()
		return bytes.Contains(ra, []byte("alice")) && bytes.Contains(rb, []byte("bob"))
	})

	seenA, gotA := otsA.snapshot()
	seenB, gotB := otsB.snapshot()
	if bytes.Contains(gotA, []byte("bob")) {
		t.Error("tenant A's server received tenant B's traffic")
	}
	if bytes.Contains(gotB, []byte("alice")) {
		t.Error("tenant B's server received tenant A's traffic")
	}
	// Upstream, a whole tenant is ONE OpenTAKServer user: the phones keep their
	// own certificates and the front never holds their keys.
	for _, seen := range [][]string{seenA, seenB} {
		if len(seen) != 1 || seen[0] != "meshsatproxy" {
			t.Errorf("upstream saw %v, want one connection as meshsatproxy", seen)
		}
	}
	_, opened, _ := f.rec.state()
	if len(opened) != 2 {
		t.Errorf("opened = %v, want one per phone", opened)
	}
}

// Two phones in one tenant must still see each other. They arrive as separate
// connections sharing one upstream identity, and the tenant's server relays
// between them — including when the two phones are on different Hub replicas,
// which is the same shape as two connections from one replica.
func TestPhonesInOneTenantRelayThroughTheirServer(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})

	one, err := f.dial(t, ca.issue(t, "alice"))
	if err != nil {
		t.Fatalf("dial one: %v", err)
	}
	two, err := f.dial(t, ca.issue(t, "bravo"))
	if err != nil {
		t.Fatalf("dial two: %v", err)
	}
	// Let both reach the upstream before sending, so the relay has someone to
	// relay to.
	waitFor(t, "both streams upstream", func() bool {
		seen, _ := ots.snapshot()
		return len(seen) == 2
	})

	if _, err := one.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := two.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := two.Read(buf)
	if err != nil {
		t.Fatalf("second phone read: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("alice")) {
		t.Errorf("second phone got %q, want the first phone's event", buf[:n])
	}
}

// The Hub runs two replicas and a phone may land on either, so the front must
// need no coordination between them: each replica opens its own connection to
// the tenant's server under that tenant's identity, and the server relays
// between those connections. Two independent fronts here stand for two
// replicas, which is the only thing that distinguishes them.
func TestPhonesOnDifferentFrontsStillSeeEachOther(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	replicaOne := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})
	replicaTwo := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})
	if replicaOne.addr == replicaTwo.addr {
		t.Fatal("the two fronts share a listener, so this proves nothing")
	}

	alice, err := replicaOne.dial(t, ca.issue(t, "alice"))
	if err != nil {
		t.Fatalf("alice on the first front: %v", err)
	}
	bravo, err := replicaTwo.dial(t, ca.issue(t, "bravo"))
	if err != nil {
		t.Fatalf("bravo on the second front: %v", err)
	}
	waitFor(t, "both fronts to reach the tenant's server", func() bool {
		seen, _ := ots.snapshot()
		return len(seen) == 2
	})

	if _, err := alice.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := bravo.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	relayed := make([]byte, 256)
	n, err := bravo.Read(relayed)
	if err != nil {
		t.Fatalf("read on the other front: %v", err)
	}
	if !bytes.Contains(relayed[:n], []byte("alice")) {
		t.Errorf("the other front's phone got %q, want alice's event", relayed[:n])
	}
}

// The front carries bytes and never parses them: ATAK negotiates between CoT
// XML and a protobuf stream on this socket, so anything that assumed lines, or
// XML, would break one of the two.
func TestBytesArriveExactlyAsSent(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})

	blob := make([]byte, 70*1024) // larger than the 32 KiB copy buffer
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("random: %v", err)
	}
	blob[0], blob[1], blob[2] = 0xbf, 0x01, 0x00 // no newline, NUL bytes inside

	conn, err := f.dial(t, ca.issue(t, "alice"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write(blob); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "every byte to arrive", func() bool {
		_, got := ots.snapshot()
		return len(got) >= len(blob)
	})
	if _, got := ots.snapshot(); !bytes.Equal(got, blob) {
		t.Errorf("upstream received %d bytes, sent %d, and they differ", len(got), len(blob))
	}
}

func TestACertificateFromAnUnknownCANeverReachesATenant(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})

	stranger := newTestCA(t, "stranger-ca")
	conn, err := f.dial(t, stranger.issue(t, "mallory"))
	if err == nil {
		// TLS 1.3 may finish the client handshake before the server's alert
		// arrives; the first read must then fail.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, rerr := conn.Read(make([]byte, 1)); rerr == nil {
			t.Fatal("a certificate from an unknown CA was served")
		}
	}
	waitFor(t, "the refusal to be recorded", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})
	refused, opened, _ := f.rec.state()
	if refused[0] != "/"+reasonHandshake {
		t.Errorf("refused = %v, want an unattributed handshake refusal", refused)
	}
	if len(opened) != 0 {
		t.Errorf("opened = %v, want none", opened)
	}
	if seen, got := ots.snapshot(); len(seen) != 0 || len(got) != 0 {
		t.Errorf("upstream saw %v and %d bytes, want neither", seen, len(got))
	}
}

// OpenTAKServer publishes no CRL and ATAK would not read one, so a certificate
// cannot say whether its user is still allowed. The Authorizer is asked on every
// connection, and the server must not exist without one.
func TestARemovedUserIsRefusedDespiteAValidCertificate(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	denied := errors.New("user was removed")
	authz := denyFunc(func(_, cn string) error {
		if cn == "gone" {
			return denied
		}
		return nil
	})
	f := startFront(t, Config{}, authz, []Tenant{tenantOn(t, "aaa", ca, ots)})

	conn, err := f.dial(t, ca.issue(t, "gone"))
	if err != nil {
		t.Fatalf("dial: %v", err) // the handshake succeeds; the stream does not
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a removed user was served")
	}
	waitFor(t, "the refusal", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})
	refused, _, _ := f.rec.state()
	if refused[0] != "aaa/"+reasonUnauthorized {
		t.Errorf("refused = %v, want aaa/%s", refused, reasonUnauthorized)
	}
	if seen, _ := ots.snapshot(); len(seen) != 0 {
		t.Errorf("upstream saw %v, want nothing", seen)
	}

	// A user still on the list goes through, proving the refusal was the
	// authorizer and not a broken certificate.
	if _, err := f.dial(t, ca.issue(t, "alice")); err != nil {
		t.Fatalf("allowed user dial: %v", err)
	}
	waitFor(t, "the allowed user upstream", func() bool {
		seen, _ := ots.snapshot()
		return len(seen) == 1
	})
}

// A phone on a mobile network reconnects constantly, and a reconnect may resume
// the TLS session. Go does not call VerifyPeerCertificate on a resumed session,
// so resolving the tenant there would refuse every reconnecting phone; this is
// the regression test for that (gosec G123).
func TestAResumedSessionIsStillAttributedToItsTenant(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenantOn(t, "aaa", ca, ots)})

	cert := ca.issue(t, "alice")
	cache := tls.NewLRUClientSessionCache(8)
	dial := func() *tls.Conn {
		t.Helper()
		conn, err := tls.Dial("tcp", f.addr, &tls.Config{
			Certificates:       []tls.Certificate{cert},
			RootCAs:            f.platformCA.pool(),
			ServerName:         "tak.test",
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: cache,
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	first := dial()
	if _, err := first.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// The session ticket arrives as its own record after the handshake, and the
	// client only takes it in while reading, so read until the deadline.
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = first.Read(make([]byte, 1))
	waitFor(t, "the first stream", func() bool {
		_, opened, _ := f.rec.state()
		return len(opened) == 1
	})
	_ = first.Close()

	second := dial()
	if _, err := second.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if state := second.ConnectionState(); !state.DidResume {
		t.Fatalf("the second connection did not resume (TLS version 0x%04x), so this proves nothing; "+
			"a per-connection tls.Config clone is the usual cause, since ticket keys are per Config",
			state.Version)
	}
	waitFor(t, "the resumed stream", func() bool {
		_, opened, _ := f.rec.state()
		return len(opened) == 2
	})
	refused, opened, _ := f.rec.state()
	if opened[1] != "aaa/alice" {
		t.Errorf("resumed stream attributed to %q, want aaa/alice", opened[1])
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
}

func TestServerRefusesToExistWithoutAnAuthorizer(t *testing.T) {
	ca := newTestCA(t, "platform")
	_, err := NewServer(Config{Certificate: ca.issue(t, "tak.meshsat.net", forDNS("tak.test"))}, nil, nil)
	if !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("err = %v, want ErrNoAuthorizer", err)
	}
	if _, err := NewServer(Config{}, allowAll{}, nil); err == nil {
		t.Fatal("NewServer accepted no server certificate")
	}
}

// Every upstream connection forks a process in OpenTAKServer, so one tenant
// must not be able to spend the front or its own server.
func TestTheTenantConnectionCeilingHolds(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	tenant := tenantOn(t, "aaa", ca, ots)
	tenant.MaxConns = 1
	f := startFront(t, Config{}, allowAll{}, []Tenant{tenant})

	if _, err := f.dial(t, ca.issue(t, "alice")); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	waitFor(t, "the first stream", func() bool {
		_, opened, _ := f.rec.state()
		return len(opened) == 1
	})

	second, err := f.dial(t, ca.issue(t, "bravo"))
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("the second connection was served past the ceiling")
	}
	// The connection is closed before the refusal is recorded, so a client that
	// has already seen the close has not necessarily been counted yet.
	waitFor(t, "the refusal to be recorded", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})
	refused, opened, _ := f.rec.state()
	if refused[0] != "aaa/"+reasonTenantFull {
		t.Errorf("refused = %v, want aaa/%s", refused, reasonTenantFull)
	}
	if len(opened) != 1 {
		t.Errorf("opened = %v, want only the first", opened)
	}
	// One phone still holds the tenant's single slot.
	if _, tenantConns := f.srv.Conns("aaa"); tenantConns != 1 {
		t.Fatalf("tenant conns = %d, want 1", tenantConns)
	}
}

func TestAnIdleStreamIsClosed(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{IdleTimeout: 150 * time.Millisecond}, allowAll{},
		[]Tenant{tenantOn(t, "aaa", ca, ots)})

	conn, err := f.dial(t, ca.issue(t, "alice"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("<event uid=\"alice\"/>\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("an idle stream stayed open")
	} else if !errors.Is(err, io.EOF) && !isClosed(err) {
		t.Logf("idle close surfaced as %v", err) // any close is acceptable
	}
	waitFor(t, "the stream to be reported closed", func() bool {
		_, _, closed := f.rec.state()
		return closed == 1
	})
}

func isClosed(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, net.ErrClosed)
}

// An upstream that is not there must be a refusal, not a hang.
func TestAnUnreachableTenantServerIsRefused(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	tenant := tenantOn(t, "aaa", ca, ots)
	tenant.Upstream = "127.0.0.1:1" // nothing listens here
	f := startFront(t, Config{DialTimeout: time.Second}, allowAll{}, []Tenant{tenant})

	conn, err := f.dial(t, ca.issue(t, "alice"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("served despite no upstream")
	}
	waitFor(t, "the refusal to be recorded", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})
	refused, _, _ := f.rec.state()
	if refused[0] != "aaa/"+reasonUpstream {
		t.Errorf("refused = %v, want aaa/%s", refused, reasonUpstream)
	}
}

// SetDirectory is how a tenant appears or goes away without a restart.
func TestANewDirectoryTakesEffectOnTheNextConnection(t *testing.T) {
	ots := newFakeOTS(t, "a")
	ca := newTestCA(t, "tenant-a-ca")
	f := startFront(t, Config{}, allowAll{}, nil)

	if _, err := f.dial(t, ca.issue(t, "alice")); err == nil {
		t.Log("handshake completed before the alert; the read below is the check")
	}
	waitFor(t, "the unknown tenant refusal", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})

	dir, err := NewDirectory([]Tenant{tenantOn(t, "aaa", ca, ots)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	f.srv.SetDirectory(dir)
	if f.srv.Directory().Len() != 1 {
		t.Fatalf("directory did not take")
	}
	if _, err := f.dial(t, ca.issue(t, "alice")); err != nil {
		t.Fatalf("dial after the tenant appeared: %v", err)
	}
	waitFor(t, "the stream", func() bool {
		_, opened, _ := f.rec.state()
		return len(opened) == 1
	})
}

func TestConfigDefaultsAreApplied(t *testing.T) {
	got := (&Config{}).withDefaults()
	for name, ok := range map[string]bool{
		"handshake timeout": got.HandshakeTimeout > 0,
		"idle timeout":      got.IdleTimeout > 0,
		"dial timeout":      got.DialTimeout > 0,
		"max conns":         got.MaxConns > 0,
		"per tenant":        got.MaxConnsPerTenant > 0,
		"logger":            got.Logger != nil,
	} {
		if !ok {
			t.Errorf("%s has no default", name)
		}
	}
	// A negative idle timeout disables the timer rather than closing at once.
	if d := (&Config{IdleTimeout: -1}).withDefaults().IdleTimeout; d != -1 {
		t.Errorf("IdleTimeout = %v, want the -1 kept", d)
	}
}
