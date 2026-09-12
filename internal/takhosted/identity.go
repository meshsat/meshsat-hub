package takhosted

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// IdentityKeeper obtains and holds the Hub's own client certificate for each
// tenant's OpenTAKServer.
//
// # Why the Hub cannot just make one
//
// Each tenant's upstream verifies client certificates against that tenant's own
// CA, and the Hub does not have the CA key -- deliberately, because it is
// internet-facing and whoever holds that key can impersonate any phone in the
// tenant. So the Hub generates a key, keeps the private half in memory, and asks
// the operator to sign the public half by creating a TakCertificateRequest with
// purpose "hub". The operator issues the subject; the requester does not choose
// it, which is what stops a compromised Hub asking for a certificate naming a
// customer's user.
//
// # Why this is not a blocking call
//
// Issuance is asynchronous: the operator reconciles on a ticker, so a fresh
// request sits Pending for a few seconds. The directory refresh runs every five
// minutes across every replica, and blocking it on a signature would stall the
// whole tenant set for one tenant. So For returns ErrIdentityPending immediately
// and the tenant is skipped from this snapshot, appearing in the next one.
//
// # The private key must outlive the wait
//
// The key is generated before the CSR and the certificate arrives later, so the
// pending key is held until its certificate shows up. Losing it between the two
// would produce a certificate nobody holds the key for, and the Hub would ask
// again forever.
type IdentityKeeper struct {
	client *Client
	log    *slog.Logger
	now    func() time.Time

	mu sync.Mutex
	// held is the usable identity per tenant.
	held map[string]*heldIdentity
	// pending keys, by tenant, waiting for a signature.
	pending map[string]*pendingKey
}

type heldIdentity struct {
	pair     tls.Certificate
	notAfter time.Time
	serial   string
}

type pendingKey struct {
	keyPEM  []byte
	created time.Time
}

// ErrIdentityPending means the certificate has been asked for and is not signed
// yet. The caller should skip this tenant and try on the next refresh.
var ErrIdentityPending = errors.New("takhosted: upstream identity is pending issuance")

// ErrIdentityDenied means the operator refused to sign. This does not fix itself.
var ErrIdentityDenied = errors.New("takhosted: upstream identity was denied")

const (
	// renewBefore is how long before expiry a replacement is requested. The
	// operator issues Hub certificates for 7 days (HubValidDays), so two days of
	// headroom leaves room for several refresh cycles and a cluster that is busy.
	renewBefore = 48 * time.Hour

	// pendingPatience bounds how long a request may sit unsigned before the Hub
	// gives up on it and asks again with a fresh key. Without this, an object
	// somebody deleted by hand would leave the tenant waiting forever.
	pendingPatience = 10 * time.Minute
)

// NewIdentityKeeper wires one up.
func NewIdentityKeeper(c *Client, log *slog.Logger) *IdentityKeeper {
	if log == nil {
		log = slog.Default()
	}
	return &IdentityKeeper{
		client:  c,
		log:     log,
		now:     time.Now,
		held:    map[string]*heldIdentity{},
		pending: map[string]*pendingKey{},
	}
}

// certRequestName is the object name for a tenant's Hub identity request.
//
// Deterministic, so a retry lands on the same object instead of queueing a second
// signature, and derived from the opaque label rather than the tenant id: a
// certificate request name is visible in the namespace, and the label leaks
// nothing about who the customer is.
func certRequestName(label string) string { return "hub-" + label }

// For returns the Hub's certificate for one tenant, asking for one if needed.
//
// It never blocks on issuance. The three outcomes are: a usable certificate, a
// pending one (ErrIdentityPending), or a refusal (ErrIdentityDenied).
func (k *IdentityKeeper) For(ctx context.Context, tenantID, label string) (tls.Certificate, error) {
	if k == nil || k.client == nil {
		return tls.Certificate{}, errors.New("takhosted: identity keeper has no client")
	}
	if label == "" {
		return tls.Certificate{}, errors.New("takhosted: tenant has no label")
	}

	k.mu.Lock()
	if h, ok := k.held[tenantID]; ok && k.now().Before(h.notAfter.Add(-renewBefore)) {
		pair := h.pair
		k.mu.Unlock()
		return pair, nil
	}
	k.mu.Unlock()

	// Either there is no certificate yet, or the one held is inside its renewal
	// window. Both are handled the same way: make sure a request exists and see
	// whether it has been signed.
	pair, err := k.collect(ctx, tenantID, label)
	if err == nil {
		return pair, nil
	}
	// While renewing, keep serving the certificate still in hand: it is valid for
	// up to another two days, and dropping a tenant from the directory because a
	// renewal is mid-flight would disconnect phones for no reason.
	if errors.Is(err, ErrIdentityPending) {
		k.mu.Lock()
		h, ok := k.held[tenantID]
		k.mu.Unlock()
		if ok && k.now().Before(h.notAfter) {
			return h.pair, nil
		}
	}
	return tls.Certificate{}, err
}

// collect ensures a request exists for the tenant and gathers the result.
func (k *IdentityKeeper) collect(ctx context.Context, tenantID, label string) (tls.Certificate, error) {
	name := certRequestName(label)

	req, err := k.client.GetCertRequest(ctx, name)
	switch {
	case err == nil:
		// A request exists. Does it match a key we still hold?
	case IsNotFound(err):
		return tls.Certificate{}, k.ask(ctx, tenantID, label, name)
	default:
		return tls.Certificate{}, fmt.Errorf("takhosted: reading the certificate request: %w", err)
	}

	k.mu.Lock()
	pk, havePending := k.pending[tenantID]
	k.mu.Unlock()

	switch req.Status.Phase {
	case CertIssued:
		if !havePending {
			// The certificate is signed but the key that matches it is gone --
			// this replica restarted while the request was in flight. Start over
			// with a fresh key rather than keep a certificate nobody can use.
			k.log.Info("takhosted: an issued certificate has no matching key on this replica, asking again",
				"tenant", tenantID)
			if err := k.client.DeleteCertRequest(ctx, name); err != nil {
				return tls.Certificate{}, fmt.Errorf("takhosted: clearing a stale request: %w", err)
			}
			return tls.Certificate{}, k.ask(ctx, tenantID, label, name)
		}
		pair, err := loadPair([]byte(req.Status.CertPEM), pk.keyPEM)
		if err != nil {
			return tls.Certificate{}, err
		}
		leaf, err := parseOneCert(req.Status.CertPEM)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("takhosted: issued certificate does not parse: %w", err)
		}
		k.mu.Lock()
		k.held[tenantID] = &heldIdentity{pair: pair, notAfter: leaf.NotAfter, serial: req.Status.Serial}
		delete(k.pending, tenantID)
		k.mu.Unlock()
		k.log.Info("takhosted: upstream identity issued",
			"tenant", tenantID, "serial", req.Status.Serial, "not_after", leaf.NotAfter.UTC())
		// The request has served its purpose. Leaving it would accumulate one
		// object per tenant per renewal, each holding a certificate that is
		// public but still names the Hub.
		if err := k.client.DeleteCertRequest(ctx, name); err != nil {
			k.log.Warn("takhosted: could not clear a collected certificate request",
				"tenant", tenantID, "error", err)
		}
		return pair, nil

	case CertDenied:
		return tls.Certificate{}, fmt.Errorf("%w: %s", ErrIdentityDenied, req.Status.Message)

	default:
		// Pending, or a phase this Hub does not know. If the wait has gone on too
		// long the object is probably orphaned, so clear it and ask again.
		if havePending && k.now().Sub(pk.created) > pendingPatience {
			k.log.Warn("takhosted: certificate request unsigned for too long, asking again",
				"tenant", tenantID, "waited", k.now().Sub(pk.created).Round(time.Second))
			if err := k.client.DeleteCertRequest(ctx, name); err != nil {
				return tls.Certificate{}, fmt.Errorf("takhosted: clearing a stuck request: %w", err)
			}
			return tls.Certificate{}, k.ask(ctx, tenantID, label, name)
		}
		if !havePending {
			// Somebody else's request, or one from a previous process. Replace it
			// with ours so the key and the certificate belong together.
			if err := k.client.DeleteCertRequest(ctx, name); err != nil {
				return tls.Certificate{}, fmt.Errorf("takhosted: clearing a foreign request: %w", err)
			}
			return tls.Certificate{}, k.ask(ctx, tenantID, label, name)
		}
		return tls.Certificate{}, ErrIdentityPending
	}
}

// ask generates a key, submits a CSR and records the pending key.
func (k *IdentityKeeper) ask(ctx context.Context, tenantID, label, name string) error {
	csrPEM, keyPEM, err := newHubCSR()
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.pending[tenantID] = &pendingKey{keyPEM: keyPEM, created: k.now()}
	k.mu.Unlock()

	err = k.client.CreateCertRequest(ctx, TakCertificateRequest{
		Metadata: objectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/name":  "meshsat-hub",
				"tak.meshsat.net/label":   label,
				"tak.meshsat.net/purpose": PurposeHub,
			},
		},
		Spec: CertReqSpec{
			Label:    label,
			Purpose:  PurposeHub,
			Username: HubIdentity,
			CSRPEM:   string(csrPEM),
		},
	})
	if err != nil {
		k.mu.Lock()
		delete(k.pending, tenantID)
		k.mu.Unlock()
		return fmt.Errorf("takhosted: asking for an upstream certificate: %w", err)
	}
	k.log.Info("takhosted: asked for an upstream identity", "tenant", tenantID, "label", label)
	return ErrIdentityPending
}

// newHubCSR makes a key and a certificate request for the Hub's identity.
//
// ECDSA P-256 rather than the RSA the tenant CA itself uses. The RSA choice there
// is about the TAK ecosystem: a phone's certificate ends up in a p12 and in
// ATAK's truststore, which are RSA by convention. This certificate goes into one
// TLS handshake between two of our own processes, so the smaller, faster key is
// the better one, and the operator's issuer accepts P-256.
//
// The subject is set to the Hub identity for readability only -- the operator
// DISCARDS the CSR subject and issues the username from the spec. That is the
// whole point of asking rather than self-signing, and a reader should not have to
// wonder which one wins.
func newHubCSR() (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("takhosted: generate key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: HubIdentity},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("takhosted: create CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("takhosted: marshal key: %w", err)
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return csrPEM, keyPEM, nil
}

// Holding reports which tenants have a usable identity and when each expires,
// for the readiness probe.
func (k *IdentityKeeper) Holding() map[string]time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]time.Time, len(k.held))
	for id, h := range k.held {
		out[id] = h.notAfter
	}
	return out
}

// Forget drops a tenant's identity and any pending key, for a purge.
func (k *IdentityKeeper) Forget(tenantID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.held, tenantID)
	delete(k.pending, tenantID)
}
