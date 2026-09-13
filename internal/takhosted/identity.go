package takhosted

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
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
	// replica identifies THIS process. It is part of every request's object name,
	// which is what stops two replicas fighting over one certificate. See
	// certRequestName.
	replica string

	mu sync.Mutex
	// held is the usable identity per tenant.
	held map[string]*heldIdentity
	// pending keys, by tenant, waiting for a signature.
	pending map[string]*pendingKey
	// refused records the tenants whose request the operator turned down, so the
	// Hub can try again on a backoff instead of reading the same refusal forever
	// (MESHSAT-1075).
	refused map[string]*refusal
}

// refusal is what the Hub remembers about a turned-down request.
type refusal struct {
	// at is when the last attempt was refused, and tries how many have been.
	// Together they pace the retry; without them a five-minute refresh would
	// delete and re-ask every five minutes for as long as the cause persisted.
	at     time.Time
	tries  int
	reason string
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

// ErrIdentityDenied means the operator refused to sign.
//
// It used to carry the sentence "This does not fix itself", which was an accurate
// description of a defect rather than a design (MESHSAT-1075): a refused request
// was left in place and re-read on every refresh, so the tenant was skipped
// forever and the only cure was a person deleting the object by hand. The Hub now
// clears a refusal and asks again on a backoff, so this error means "not right
// now" rather than "never".
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

	// A refused request is retried, because the commonest causes are transient --
	// an instance that was not Ready yet, a CRD mid-rollout, a validator that has
	// since been corrected. The first retry is immediate for exactly that reason:
	// when the front was first enabled, every tenant was refused by a validator
	// that a later merge fixed, and nothing made the Hub ask again.
	//
	// After that it backs off, because a genuinely malformed request would
	// otherwise mean one delete and one create per tenant per refresh forever.
	denialRetryBase = 5 * time.Minute
	denialRetryMax  = time.Hour
)

// retryAfter is how long to wait before asking again, having been refused tries
// times already. Doubling from denialRetryBase, capped.
func retryAfter(tries int) time.Duration {
	if tries <= 1 {
		return 0
	}
	d := denialRetryBase
	for i := 2; i < tries; i++ {
		d *= 2
		if d >= denialRetryMax {
			return denialRetryMax
		}
	}
	return d
}

// NewIdentityKeeper wires one up.
//
// replica identifies this process and MUST differ between replicas; pass the pod
// name. An empty string is replaced with random bytes rather than a shared
// default, because a shared name is the defect this parameter exists to prevent
// (MESHSAT-1076) and a misconfigured POD_NAME must not silently reintroduce it.
// The cost of a random one is an abandoned request object per restart, which the
// operator reaps.
func NewIdentityKeeper(c *Client, replica string, log *slog.Logger) *IdentityKeeper {
	if log == nil {
		log = slog.Default()
	}
	if replica == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Even this is survivable: the clock is unique enough to keep two
			// replicas apart, and failing to start over it would be worse.
			replica = fmt.Sprintf("anon-%d", time.Now().UnixNano())
		} else {
			replica = "anon-" + hex.EncodeToString(b[:])
		}
		log.Warn("takhosted: no replica id given for the upstream identity, using a random one",
			"replica", replica)
	}
	return &IdentityKeeper{
		client:  c,
		log:     log,
		now:     time.Now,
		replica: replica,
		held:    map[string]*heldIdentity{},
		pending: map[string]*pendingKey{},
		refused: map[string]*refusal{},
	}
}

// certRequestName is the object name for one replica's identity request for one
// tenant.
//
// # Why the replica is in the name (MESHSAT-1076)
//
// It used to be just "hub-"+label: one object for every replica. But the private
// key that matches the certificate lives in ONE replica's memory, on purpose --
// it is never written down. So the replica that did not ask looks at the signed
// certificate, finds it holds no matching key, concludes it must have restarted
// mid-flight, deletes the certificate and asks again. The other replica then does
// the same. Two replicas delete each other's certificates forever, no tenant ever
// enters the front's directory, and not one phone can connect. Found on production
// the day the front was first switched on; every pipeline was green throughout.
//
// One object per replica per tenant makes that structurally impossible, and it
// keeps the property the rest of this design rests on: the key stays in memory.
// The alternative -- a leader mints one identity and shares it through a Secret --
// would put a key that authenticates to every tenant's admin gateway at rest in
// etcd, which this cluster stores unencrypted and Velero copies to the object
// store, and would force the Hub's Role to gain Secret read in meshsat-tak, which
// the release gate asserts is closed. It would also put a leader on the serving
// path, when the whole point of two replicas is that either can serve alone.
//
// The replica is hashed rather than spelled out to keep the name short and
// certainly valid; the readable form goes in a label so `kubectl get -L` can
// answer "whose is this?".
func certRequestName(label, replica string) string {
	sum := sha256.Sum256([]byte(replica))
	return "hub-" + label + "-" + hex.EncodeToString(sum[:4])
}

// replicaLabel is the full replica id, trimmed to what a label value allows, so
// an operator can see which pod owns a request.
func replicaLabel(replica string) string {
	if len(replica) > 63 {
		return replica[:63]
	}
	return replica
}

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
	name := certRequestName(label, k.replica)

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
			//
			// This branch is only SOUND because the object name carries the
			// replica (MESHSAT-1076). While the name was shared, "I hold no key
			// for this" was also true for a certificate a DIFFERENT replica had
			// just been issued, so this deleted a working certificate and the two
			// replicas destroyed each other's forever. With one object per
			// replica the condition means what it says.
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
		// Whatever was refused before has been superseded: start the backoff from
		// nothing if this tenant is ever refused again.
		delete(k.refused, tenantID)
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
		return tls.Certificate{}, k.afterRefusal(ctx, tenantID, label, name, req.Status.Message)

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

// afterRefusal decides what to do about a request the operator turned down.
//
// # The defect this replaces (MESHSAT-1075)
//
// A refusal used to be returned and nothing else. The object stayed, so every
// refresh read the same Denied status and returned the same error: the tenant was
// skipped from the front's directory for good, no phone in it could connect, and
// the only cure was a person noticing and deleting the object by hand. There was
// no metric and one WARN line per refresh.
//
// That is the wrong default, because the causes seen in practice are transient.
// When the front was first switched on, EVERY tenant was refused -- by a username
// validator that a later merge corrected -- and the Hub would have stayed dead
// after the fix landed.
//
// So a refusal now clears the object and asks again, on a backoff so that a
// genuinely bad request does not mean one delete and one create per tenant per
// refresh for ever. The first retry is immediate, the rest double from five
// minutes to an hour.
//
// It is logged at ERROR, not WARN, and the line says what it costs: for as long
// as this lasts the tenant is absent from the front and its phones cannot connect.
// Once per decision, not once per refresh.
func (k *IdentityKeeper) afterRefusal(ctx context.Context, tenantID, label, name, why string) error {
	refusedErr := fmt.Errorf("%w: %s", ErrIdentityDenied, why)

	k.mu.Lock()
	r := k.refused[tenantID]
	if r == nil {
		r = &refusal{}
		k.refused[tenantID] = r
	}
	wait := retryAfter(r.tries + 1)
	due := r.tries == 0 || k.now().Sub(r.at) >= wait
	tries := r.tries
	k.mu.Unlock()

	identityRefused.Inc()

	if !due {
		// Already asked recently. Stay quiet: the directory's skipped gauge and
		// this counter are what make the condition visible, and a log line per
		// refresh per tenant is how a new path fills a disk.
		k.log.Debug("takhosted: upstream identity still refused, waiting before asking again",
			"tenant", tenantID, "reason", why, "attempts", tries)
		return refusedErr
	}

	k.log.Error("takhosted: the operator REFUSED this tenant's upstream identity -- "+
		"while this lasts the tenant is absent from the TAK front and none of its phones can connect; asking again",
		"tenant", tenantID, "reason", why, "attempt", tries+1,
		"next_attempt_after", retryAfter(tries+2).String())

	if err := k.client.DeleteCertRequest(ctx, name); err != nil {
		// Leave the bookkeeping alone so the next refresh retries the delete
		// rather than counting this as an attempt that happened.
		return fmt.Errorf("takhosted: clearing a refused request: %w", err)
	}

	k.mu.Lock()
	r.tries++
	r.at = k.now()
	r.reason = why
	// The key that went with the refused request is of no further use; ask()
	// replaces it, but dropping it here keeps the two maps from disagreeing about
	// what is in flight.
	delete(k.pending, tenantID)
	k.mu.Unlock()

	if err := k.ask(ctx, tenantID, label, name); err != nil {
		return err
	}
	// Asked again, so the caller should treat this as pending rather than as a
	// refusal -- otherwise a single refusal reads as permanent to every layer above.
	return ErrIdentityPending
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
				// Which replica this belongs to. The object NAME carries a hash of
				// it; this is the readable form, so `kubectl get -L
				// tak.meshsat.net/replica` answers "whose is this?" without
				// anyone having to recompute a digest.
				"tak.meshsat.net/replica": replicaLabel(k.replica),
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

// ForgetTenant satisfies tenancy.TenantForgetter so a purge or a closure
// actually reaches this cache (MESHSAT-1109).
//
// This is the one that most needed it. The others expire on their own within
// seconds; a held identity survives until the certificate's renewal window,
// which is weeks -- so before this, a purged tenant kept a usable hosted-TAK
// identity resident in every replica.
func (k *IdentityKeeper) ForgetTenant(tenantID string) {
	if k == nil {
		return
	}
	k.Forget(tenantID)
}

// Forget drops a tenant's identity and any pending key, for a purge.
func (k *IdentityKeeper) Forget(tenantID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.held, tenantID)
	delete(k.pending, tenantID)
	// The refusal goes too. A tenant that is forgotten and later reappears is, as
	// far as this keeper is concerned, a new tenant; making it serve out a backoff
	// earned by a previous incarnation would delay it for no reason.
	delete(k.refused, tenantID)
}
