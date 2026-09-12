package takfront

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Limits a connection can run into before it reaches a tenant's server.
var (
	// ErrServerFull means the front is at its overall connection limit.
	ErrServerFull = errors.New("takfront: server connection limit reached")
	// ErrTenantFull means this tenant is at its own limit. It is per tenant so
	// that one customer cannot spend the whole front, and because every
	// upstream forks a process per connection.
	ErrTenantFull = errors.New("takfront: tenant connection limit reached")
	// ErrNoAuthorizer means the server was started without a way to check
	// whether a user is still allowed. It fails closed rather than trusting
	// the certificate alone.
	ErrNoAuthorizer = errors.New("takfront: refusing to serve without an authorizer")
)

// Authorizer decides whether a phone whose certificate verified may still
// stream.
//
// The certificate proves which tenant and user it was issued to. It cannot
// prove the user has not since been removed: OpenTAKServer publishes no CRL,
// and ATAK would not fetch one. So revocation is asked here instead, once per
// connection, against whoever issued the certificate.
type Authorizer interface {
	AllowTAKClient(ctx context.Context, tenantID, commonName string, serial *big.Int) error
}

// Recorder observes connection outcomes. Implementations carry the metrics and
// the audit log; the server itself only logs. Every method must be safe for
// concurrent use and must not block: they run on the connection's own
// goroutine.
type Recorder interface {
	// Refused is called when a connection never reached a tenant. tenantID is
	// empty when the certificate did not resolve to one.
	Refused(ctx context.Context, tenantID, reason string, remote net.Addr)
	// Opened is called once a phone is proxied to its tenant.
	Opened(ctx context.Context, p *Peer, remote net.Addr)
	// Closed reports the finished connection. up counts bytes from the phone
	// to the tenant's server, down the other way.
	Closed(ctx context.Context, p *Peer, up, down int64, d time.Duration, err error)
}

// Config is everything the front needs that is not per tenant.
type Config struct {
	// Certificate is the ONE server certificate every phone sees.
	//
	// Without SNI the front must present a certificate before it knows which
	// tenant is calling, so this cannot be per tenant: each tenant's truststore
	// has to carry this certificate's chain. ATAK does not check the hostname
	// in it (commoncommo skips hostname verification deliberately), but it does
	// check the chain.
	Certificate tls.Certificate
	// HandshakeTimeout bounds how long a connection may stay unidentified.
	// Zero means 15s.
	HandshakeTimeout time.Duration
	// IdleTimeout closes a stream that carries nothing in either direction.
	// ATAK sends a position report every few seconds, so this can be short.
	// Zero means 10m; negative disables it.
	IdleTimeout time.Duration
	// DialTimeout bounds connecting to a tenant's server. Zero means 10s.
	DialTimeout time.Duration
	// MaxConns caps connections across all tenants. Zero means 2000.
	MaxConns int
	// MaxConnsPerTenant is the default tenant cap, used when a Tenant does not
	// set its own. Zero means 100.
	MaxConnsPerTenant int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.HandshakeTimeout == 0 {
		out.HandshakeTimeout = 15 * time.Second
	}
	if out.IdleTimeout == 0 {
		out.IdleTimeout = 10 * time.Minute
	}
	if out.DialTimeout == 0 {
		out.DialTimeout = 10 * time.Second
	}
	if out.MaxConns == 0 {
		out.MaxConns = 2000
	}
	if out.MaxConnsPerTenant == 0 {
		out.MaxConnsPerTenant = 100
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	return out
}

// Server is the TAK front: one listener, every tenant.
type Server struct {
	cfg     Config
	authz   Authorizer
	rec     Recorder
	baseTLS *tls.Config

	dir atomic.Pointer[Directory]

	conns  atomic.Int64
	mu     sync.Mutex
	perTen map[string]int

	wg sync.WaitGroup
}

// NewServer builds a front. It refuses to exist without an Authorizer, because
// a certificate alone cannot say whether a user is still allowed.
func NewServer(cfg Config, authz Authorizer, rec Recorder) (*Server, error) {
	if authz == nil {
		return nil, ErrNoAuthorizer
	}
	if len(cfg.Certificate.Certificate) == 0 {
		return nil, errors.New("takfront: no server certificate")
	}
	s := &Server{
		cfg:    cfg.withDefaults(),
		authz:  authz,
		rec:    rec,
		perTen: make(map[string]int),
	}
	s.dir.Store(&Directory{byIssuer: map[string]*entry{}})
	s.baseTLS = &tls.Config{
		Certificates: []tls.Certificate{cfg.Certificate},
		MinVersion:   tls.VersionTLS12,
		// Any certificate, verified by us: the tenant's CA is chosen from the
		// issuer, so stock verification against a union of every tenant CA
		// would prove nothing about which tenant a phone belongs to.
		ClientAuth: tls.RequireAnyClientCert,
		// VerifyConnection, NOT VerifyPeerCertificate: Go skips
		// VerifyPeerCertificate on a RESUMED session, and ATAK reconnects
		// constantly, so a check that lives there is a check that a reconnecting
		// phone can miss (gosec G123). This refuses the handshake outright;
		// serve reads the same answer back off the finished connection to learn
		// which tenant to dial.
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := s.dir.Load().ResolveChain(cs.PeerCertificates, time.Now())
			return err
		},
	}
	return s, nil
}

// SetDirectory publishes a new tenant snapshot. Connections already open keep
// the tenant they started with; new ones see the new set immediately.
func (s *Server) SetDirectory(d *Directory) {
	if d == nil {
		return
	}
	s.dir.Store(d)
}

// Directory returns the current snapshot.
func (s *Server) Directory() *Directory { return s.dir.Load() }

// Serve accepts connections until ctx is cancelled or ln fails, then waits for
// the connections it started.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	s.cfg.Logger.Info("takfront: listening", "addr", ln.Addr().String(),
		"tenants", s.dir.Load().Len())

	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			// A per-process descriptor limit is transient: backing off beats
			// spinning the accept loop, and beats killing the listener.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				s.cfg.Logger.Warn("takfront: accept failed", "error", err, "retry_in", delay)
				time.Sleep(delay)
				continue
			}
			s.wg.Wait()
			return fmt.Errorf("takfront: accept: %w", err)
		}
		delay = 0
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(ctx, conn)
		}()
	}
}

func (s *Server) serve(ctx context.Context, raw net.Conn) {
	remote := raw.RemoteAddr()
	if total := s.conns.Add(1); int(total) > s.cfg.MaxConns {
		s.conns.Add(-1)
		s.refuse(ctx, raw, "", reasonServerFull, remote, ErrServerFull)
		return
	}
	defer s.conns.Add(-1)

	// ONE shared tls.Config for every connection, never a per-connection clone.
	// Go derives session-ticket keys per Config, so a clone per connection
	// hands out tickets nothing else can decrypt and every reconnect becomes a
	// full handshake. Against a server that forks a process per connection,
	// with phones that reconnect whenever the mobile network blinks, that is
	// the expensive path. The config's VerifyConnection has already refused
	// anything it could not attribute by the time the handshake returns.
	conn := tls.Server(raw, s.baseTLS)
	if err := conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout)); err != nil {
		s.refuse(ctx, conn, "", reasonDeadline, remote, err)
		return
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		// Includes every resolution failure: unknown issuer, expired, no CN.
		s.refuse(ctx, conn, "", reasonHandshake, remote, err)
		return
	}
	// The answer the handshake already accepted, read back off the finished
	// connection. It costs one more signature check and keeps the routing
	// decision out of a TLS callback, where a resumed session could skip it.
	peer, err := s.dir.Load().ResolveChain(conn.ConnectionState().PeerCertificates, time.Now())
	if err != nil {
		s.refuse(ctx, conn, "", reasonUnidentified, remote, err)
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.refuse(ctx, conn, peer.Tenant.TenantID, reasonDeadline, remote, err)
		return
	}

	if err := s.authz.AllowTAKClient(ctx, peer.Tenant.TenantID, peer.CommonName, peer.Serial); err != nil {
		s.refuse(ctx, conn, peer.Tenant.TenantID, reasonUnauthorized, remote, err)
		return
	}
	if !s.claim(peer.Tenant) {
		s.refuse(ctx, conn, peer.Tenant.TenantID, reasonTenantFull, remote, ErrTenantFull)
		return
	}
	defer s.release(peer.Tenant.TenantID)

	up, err := s.dialUpstream(ctx, peer.Tenant)
	if err != nil {
		s.refuse(ctx, conn, peer.Tenant.TenantID, "upstream", remote, err)
		return
	}

	log := s.cfg.Logger.With("tenant", peer.Tenant.TenantID, "label", peer.Tenant.Label,
		"user", peer.CommonName, "remote", remote.String())
	log.Info("takfront: stream open", "upstream", peer.Tenant.Upstream)
	if s.rec != nil {
		s.rec.Opened(ctx, peer, remote)
	}

	start := time.Now()
	upBytes, downBytes, perr := s.pipe(ctx, conn, up)
	if s.rec != nil {
		s.rec.Closed(ctx, peer, upBytes, downBytes, time.Since(start), perr)
	}
	log.Info("takfront: stream closed", "up_bytes", upBytes, "down_bytes", downBytes,
		"seconds", int64(time.Since(start).Seconds()), "error", perr)
}

// DialTenant opens a connection to one tenant's OpenTAKServer as the Hub,
// for callers that need to SEND CoT rather than proxy a phone (MESHSAT-1037).
//
// It exists so internal/takhosted's outbound forwarder does not reimplement the
// TLS policy below. That policy has a subtlety worth not re-deriving: when a
// tenant sets no UpstreamServerName, the dial sets InsecureSkipVerify AND
// supplies a VerifyConnection that checks the chain against the tenant's own CA,
// so only the NAME check is skipped. A second implementation that set
// InsecureSkipVerify without the callback would accept any certificate from
// anything answering on that address, and it would look correct.
//
// It deliberately does NOT go through claim(): that budget is the tenant's
// PHONE allowance (MaxConnsPerTenant, default 100). A forwarder consuming a slot
// from it would cost every tenant one seat, and at the ceiling would refuse a
// real phone because the Hub itself was holding the last one. Accounting and
// dialing are already separate here -- claim happens at the call site, not in the
// dialer -- so this wrapper inherits that separation rather than breaking it.
//
// The caller owns the returned connection and must close it.
func (s *Server) DialTenant(ctx context.Context, t *Tenant) (net.Conn, error) {
	if t == nil {
		return nil, errors.New("takfront: no tenant")
	}
	return s.dialUpstream(ctx, t)
}

// dialUpstream connects to the tenant's server as the tenant's single identity.
func (s *Server) dialUpstream(ctx context.Context, t *Tenant) (net.Conn, error) {
	return DialUpstream(ctx, t, s.cfg.DialTimeout)
}

// DialUpstream is the upstream TLS policy, reachable without a Server.
//
// Exported for the outbound forwarder, which must also serve tenants who bring
// their OWN TAK server (MESHSAT-1065) and therefore runs whether or not the front
// is enabled -- the front needs a server certificate, and a customer pointing the
// Hub at their own endpoint needs no front at all.
//
// It exists as one function rather than two because the property that matters is
// easy to reimplement incorrectly: when UpstreamServerName is empty the chain is
// STILL verified against the tenant's own CA and only the name check is skipped,
// via VerifyConnection so it also fires on a resumed session. A second copy that
// set InsecureSkipVerify without that callback would accept any certificate from
// anything answering on the address, and would look right.
func DialUpstream(ctx context.Context, t *Tenant, timeout time.Duration) (net.Conn, error) {
	if t == nil {
		return nil, errors.New("takfront: no tenant")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return dialUpstreamWith(ctx, t, timeout)
}

func dialUpstreamWith(ctx context.Context, t *Tenant, timeout time.Duration) (net.Conn, error) {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{t.Identity},
		RootCAs:      t.UpstreamCAs,
		MinVersion:   tls.VersionTLS12,
		ServerName:   t.UpstreamServerName,
	}
	if t.UpstreamServerName == "" {
		// OpenTAKServer's own server certificate carries names that no
		// in-cluster address matches. Verify the chain against the tenant's CA
		// and skip only the name check, rather than skipping verification.
		roots := t.UpstreamCAs
		cfg.InsecureSkipVerify = true
		// VerifyConnection for the same reason as the client side: it is also
		// called when a session is resumed, which VerifyPeerCertificate is not.
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyChainOnly(cs.PeerCertificates, roots, time.Now())
		}
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    cfg,
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialer.DialContext(dctx, "tcp", t.Upstream)
	if err != nil {
		return nil, fmt.Errorf("takfront: dial %s: %w", t.Upstream, err)
	}
	return conn, nil
}

// verifyChainOnly checks a server chain against roots without checking names.
func verifyChainOnly(certs []*x509.Certificate, roots *x509.CertPool, now time.Time) error {
	if len(certs) == 0 {
		return errors.New("takfront: upstream sent no certificate")
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime:   now,
		Intermediates: x509.NewCertPool(),
	}
	for _, c := range certs[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		return fmt.Errorf("takfront: verify upstream: %w", err)
	}
	return nil
}

// pipe copies bytes both ways until one side ends, ctx is cancelled, or the
// stream goes idle.
//
// It copies bytes and never parses them: ATAK negotiates between CoT XML and
// its protobuf stream on this socket, and a proxy that understood only one of
// them would break the other. Reads are plain blocking reads with no deadline
// (MESHSAT-697): liveness is the idle timer closing the connections.
func (s *Server) pipe(ctx context.Context, down, up net.Conn) (int64, int64, error) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = down.Close()
			_ = up.Close()
		})
	}

	var timerMu sync.Mutex
	var idle *time.Timer
	if s.cfg.IdleTimeout > 0 {
		idle = time.AfterFunc(s.cfg.IdleTimeout, stop)
	}
	touch := func() {
		if idle == nil {
			return
		}
		timerMu.Lock()
		idle.Reset(s.cfg.IdleTimeout)
		timerMu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()

	type result struct {
		n   int64
		err error
	}
	upCh := make(chan result, 1)
	go func() {
		n, err := copyAndTouch(up, down, touch)
		stop() // the phone hung up: end the upstream side too
		upCh <- result{n, err}
	}()
	downN, downErr := copyAndTouch(down, up, touch)
	stop()
	u := <-upCh
	close(done)
	if idle != nil {
		idle.Stop()
	}

	err := firstRealError(u.err, downErr)
	return u.n, downN, err
}

func copyAndTouch(dst io.Writer, src io.Reader, touch func()) (int64, error) {
	buf := make([]byte, 32*1024)
	return io.CopyBuffer(dst, touchReader{src, touch}, buf)
}

// touchReader resets the idle timer on every read that carried something.
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

// firstRealError drops the errors a closed connection always produces, so that
// an ordinary hang-up is not reported as a failure.
func firstRealError(errs ...error) error {
	for _, err := range errs {
		switch {
		case err == nil, errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		default:
			return err
		}
	}
	return nil
}

func (s *Server) refuse(ctx context.Context, c net.Conn, tenantID, reason string, remote net.Addr, err error) {
	_ = c.Close()
	s.cfg.Logger.Warn("takfront: connection refused", "reason", reason, "tenant", tenantID,
		"remote", remote.String(), "error", err)
	if s.rec != nil {
		s.rec.Refused(ctx, tenantID, reason, remote)
	}
}

func (s *Server) claim(t *Tenant) bool {
	limit := t.MaxConns
	if limit <= 0 {
		limit = s.cfg.MaxConnsPerTenant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perTen[t.TenantID] >= limit {
		return false
	}
	s.perTen[t.TenantID]++
	return true
}

func (s *Server) release(tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.perTen[tenantID]; n <= 1 {
		delete(s.perTen, tenantID)
	} else {
		s.perTen[tenantID] = n - 1
	}
}

// Conns reports open connections in total and for one tenant, for tests and
// for the health endpoint.
func (s *Server) Conns(tenantID string) (total, tenant int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.conns.Load()), s.perTen[tenantID]
}
