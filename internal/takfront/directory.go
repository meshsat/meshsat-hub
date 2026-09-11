// Package takfront terminates TAK client connections for every tenant on one
// public port and proxies each into that tenant's OpenTAKServer.
//
// ATAK, iTAK and WinTAK send no SNI on the CoT streaming socket: commoncommo's
// streamingsocketmanagement.cpp sets the client certificate, the key and the
// socket and then calls SSL_connect, and the only SSL_set_tlsext_host_name in
// that library is in quicconnection.cpp, a different transport (MESHSAT-1035).
// So a hostname cannot select the tenant, and a passthrough proxy cannot read
// the client certificate either, because TLS encrypts it. The tenant has to
// come from the certificate after a handshake this process terminates: every
// tenant has its own CA, so the ISSUER names the tenant and the subject common
// name names the phone.
//
// The alternative this replaces was a public port per tenant, which is why the
// hostname in a tenant's connection string is cosmetic here and the port is
// always the same one.
package takfront

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Errors a caller may want to tell apart. Every one of them means the
// connection is refused; they differ only in what gets logged and counted.
var (
	// ErrUnknownIssuer means no tenant has the CA that signed the leaf. This
	// is the expected answer for a certificate from another deployment, a
	// decommissioned tenant, or a self-signed certificate.
	ErrUnknownIssuer = errors.New("takfront: client certificate was not issued by a known tenant CA")
	// ErrNoCertificate means the client completed no certificate. The TLS
	// handshake normally refuses this first.
	ErrNoCertificate = errors.New("takfront: client sent no certificate")
	// ErrNoCommonName means the certificate identifies no user, so the
	// connection cannot be attributed to anyone inside the tenant.
	ErrNoCommonName = errors.New("takfront: client certificate has an empty common name")
)

// Tenant is one customer's TAK instance as the front needs to see it.
//
// CA and Identity are deliberately different keys. CA verifies the phones and
// is the tenant's own client CA, whose private key the front never holds. The
// Identity is what the front presents upstream: ONE OpenTAKServer user for the
// whole tenant, never one per phone, so that no phone's private key has to be
// stored anywhere. Per-phone revocation stays with whoever issued the phone
// certificates, which is why Authorizer exists.
type Tenant struct {
	TenantID string
	// Label is the instance label, used in logs and metrics. It is not a
	// security boundary: nothing in a connection carries it.
	Label string
	// CA signs this tenant's phone certificates. Its subject must be unique
	// across tenants, which NewDirectory enforces.
	CA *x509.Certificate
	// Upstream is host:port of the tenant's OpenTAKServer EUD listener.
	Upstream string
	// Identity is the certificate the front presents to Upstream.
	Identity tls.Certificate
	// UpstreamCAs verifies the upstream's server certificate. Required:
	// without it the front would trust anything answering on Upstream.
	UpstreamCAs *x509.CertPool
	// UpstreamServerName is checked against the upstream certificate's names
	// when set. OpenTAKServer's self-signed server certificate usually carries
	// names that no in-cluster address matches, so an empty value means verify
	// the chain against UpstreamCAs and skip the name check.
	UpstreamServerName string
	// MaxConns caps simultaneous phone connections for this tenant. Zero means
	// the server's per-tenant default.
	MaxConns int
}

// Peer is a phone whose certificate verified, and the tenant it belongs to.
type Peer struct {
	Tenant     *Tenant
	CommonName string
	Serial     *big.Int
	NotAfter   time.Time
}

// Directory is an immutable snapshot of the tenants the front serves, indexed
// for lookup during a TLS handshake. Build a new one to change the set; never
// mutate a published one, because handshakes read it without locking.
type Directory struct {
	byIssuer map[string]*entry
}

type entry struct {
	tenant Tenant
	// roots holds only this tenant's CA, so a chain that verifies here cannot
	// have been signed by another tenant.
	roots *x509.CertPool
}

// NewDirectory indexes tenants by their CA's subject.
//
// Two tenants sharing a CA subject is refused rather than resolved: the issuer
// is the only thing naming the tenant, so an ambiguous one would silently route
// a phone into somebody else's instance.
func NewDirectory(tenants []Tenant) (*Directory, error) {
	d := &Directory{byIssuer: make(map[string]*entry, len(tenants))}
	for _, t := range tenants {
		switch {
		case t.TenantID == "":
			return nil, errors.New("takfront: tenant with no id")
		case t.CA == nil:
			return nil, fmt.Errorf("takfront: tenant %s has no CA", t.TenantID)
		case t.Upstream == "":
			return nil, fmt.Errorf("takfront: tenant %s has no upstream", t.TenantID)
		case len(t.Identity.Certificate) == 0:
			return nil, fmt.Errorf("takfront: tenant %s has no upstream identity", t.TenantID)
		case t.UpstreamCAs == nil:
			return nil, fmt.Errorf("takfront: tenant %s has no upstream CAs", t.TenantID)
		}
		key := string(t.CA.RawSubject)
		if other, ok := d.byIssuer[key]; ok {
			return nil, fmt.Errorf("takfront: tenants %s and %s share the CA subject %q",
				other.tenant.TenantID, t.TenantID, t.CA.Subject)
		}
		roots := x509.NewCertPool()
		roots.AddCert(t.CA)
		d.byIssuer[key] = &entry{tenant: t, roots: roots}
	}
	return d, nil
}

// Len reports how many tenants the snapshot serves.
func (d *Directory) Len() int { return len(d.byIssuer) }

// ClientCAs returns every tenant CA, for tls.Config.ClientCAs.
//
// It is only a hint to the client about acceptable issuers: the pool is the
// union of all tenants, so passing verification against it would prove nothing
// about which tenant a phone belongs to. Resolve is what decides.
func (d *Directory) ClientCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	for _, e := range d.byIssuer {
		pool.AddCert(e.tenant.CA)
	}
	return pool
}

// Resolve verifies a client's certificate chain against exactly one tenant's
// CA and reports the phone it identifies.
//
// rawCerts is the encoded form: the leaf first, then any intermediates the
// client sent. now allows tests to fix the clock.
func (d *Directory) Resolve(rawCerts [][]byte, now time.Time) (*Peer, error) {
	if len(rawCerts) == 0 {
		return nil, ErrNoCertificate
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("takfront: parse client certificate: %w", err)
		}
		certs = append(certs, c)
	}
	return d.ResolveChain(certs, now)
}

// ResolveChain is Resolve for a chain that is already parsed, which is how
// tls.Config.VerifyConnection hands it over.
func (d *Directory) ResolveChain(certs []*x509.Certificate, now time.Time) (*Peer, error) {
	if len(certs) == 0 {
		return nil, ErrNoCertificate
	}
	leaf := certs[0]
	e, ok := d.byIssuer[string(leaf.RawIssuer)]
	if !ok {
		return nil, ErrUnknownIssuer
	}
	opts := x509.VerifyOptions{
		Roots:       e.roots,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		CurrentTime: now,
	}
	if len(certs) > 1 {
		opts.Intermediates = x509.NewCertPool()
		for _, c := range certs[1:] {
			opts.Intermediates.AddCert(c)
		}
	}
	if _, err := leaf.Verify(opts); err != nil {
		// Expiry, a bad signature and a missing client-auth usage all land
		// here. The tenant is known, so it is safe to name it in the error.
		return nil, fmt.Errorf("takfront: verify against tenant %s CA: %w", e.tenant.TenantID, err)
	}
	if leaf.Subject.CommonName == "" {
		return nil, ErrNoCommonName
	}
	return &Peer{
		Tenant:     &e.tenant,
		CommonName: leaf.Subject.CommonName,
		Serial:     leaf.SerialNumber,
		NotAfter:   leaf.NotAfter,
	}, nil
}
