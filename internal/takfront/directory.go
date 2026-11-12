// Package takfront holds what the Hub needs to reach a tenant's TAK server: the
// Tenant record (its server's address, the CA its certificate must chain to, and
// the Hub's own identity for it), the Directory of tenants indexed by their CA,
// and DialUpstream, the one TLS policy for that connection.
//
// Until 11 Nov 2026 it also terminated TAK client connections for every tenant on
// one public port, :8089 (MESHSAT-1037): ATAK, iTAK and WinTAK send no SNI on the
// CoT socket (MESHSAT-1035), so the tenant came from the ISSUER of the phone's
// certificate after a handshake this process terminated, which is what the
// Directory's index by CA subject is for. That front was retired with the public
// port (MESHSAT-1466): the Hub is now the only party that talks to a TAK server,
// and kits and the MeshSat apps reach TAK through the Hub over MQTT. The
// Directory is still built by takhosted's refresher, which validates every
// tenant's material in the same pass that hands the forwarder its tenants.
package takfront

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"syscall"
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
	// DialControl, when set, is the net.Dialer Control hook for dialling
	// Upstream. Nil for a hosted instance, whose address is an in-cluster name
	// the operator chose. Set for a tenant's OWN server, whose address a
	// customer typed: there it refuses a connect to anything on the Hub's side
	// of the wire, after resolution, which is the half of that check a
	// save-time validation cannot do (MESHSAT-1460).
	DialControl func(network, address string, c syscall.RawConn) error
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
