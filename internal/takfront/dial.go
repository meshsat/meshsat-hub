package takfront

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"
)

// DialUpstream is the upstream TLS policy: how the Hub connects to a tenant's TAK
// server, hosted or the tenant's own, as that tenant's single Hub identity.
//
// Used by the outbound forwarder in internal/takhosted. What it dials is a
// Tenant from this package's directory types; the public front that used to share
// this file (one :8089 listener for every tenant's phones) was retired on
// 11 Nov 2026 with the public port (MESHSAT-1466). The Hub is now the only party
// that talks to a TAK server, so this is the only TLS that leaves for one.
//
// It exists as one function because the property that matters is easy to
// reimplement incorrectly: when UpstreamServerName is empty the chain is STILL
// verified against the tenant's own CA and only the name check is skipped, via
// VerifyConnection so it also fires on a resumed session. A second copy that set
// InsecureSkipVerify without that callback would accept any certificate from
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
	// Fail closed on a missing pool. A nil RootCAs, and a nil Roots in
	// verifyChainOnly, make crypto/x509 fall back to the SYSTEM roots, so any
	// publicly trusted certificate at the address would pass. Every builder sets
	// one today (the hosted directory and a tenant's own server, which refuses a
	// configuration without a CA); this keeps a future caller from finding out
	// the other way.
	if t.UpstreamCAs == nil {
		return nil, fmt.Errorf("takfront: no upstream CA for %s: refusing to trust the system roots", t.Label)
	}
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
		// VerifyConnection rather than VerifyPeerCertificate: it is also called
		// when a session is resumed, which VerifyPeerCertificate is not.
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyChainOnly(cs.PeerCertificates, roots, time.Now())
		}
	}
	dialer := &tls.Dialer{
		// Control is nil for a hosted instance and the request-forgery guard for
		// a tenant's own server; see Tenant.DialControl.
		NetDialer: &net.Dialer{Timeout: timeout, Control: t.DialControl},
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
	if roots == nil {
		return errors.New("takfront: no upstream CA to verify against")
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
