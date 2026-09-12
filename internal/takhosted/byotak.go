package takhosted

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Bring your own TAK server (MESHSAT-1065).
//
// A tenant who already runs TAK Server, FreeTAKServer, OpenTAKServer or taky
// points the Hub at it, and their devices' positions, SOS and telemetry are
// forwarded there as CoT alongside -- not instead of -- their hosted instance.
//
// # Why this needs no new forwarding code
//
// Forwarder does not know where it sends: it asks for upstreams and dials them.
// A customer's own server is just another upstream whose Upstream is their
// host:port, whose Identity is the client certificate they issued the Hub, and
// whose UpstreamCAs is the authority that signed their server. The loop guards,
// the zero-fix guard, connection reuse, idle reaping and redial-once all apply
// unchanged.
//
// # What this is NOT
//
// It is not the front. Their phones connect to their own server directly, so
// takfront is uninvolved and none of this depends on HUB_TAK_FRONT_ENABLED or on
// the front's server certificate.

// TAKConfigSource hands back one tenant's stored TAK settings, or nil when that
// tenant has not configured a server of their own.
//
// A one-method interface rather than an import of internal/integrations, for the
// same reason tenancy.TAKInstanceDeleter and api.TAKProvisioner exist: this
// package needs exactly one question answered, and the credential service pulls
// in the encryption and store surface that answering it happens to require.
type TAKConfigSource interface {
	TAKUpstream(ctx context.Context, tenantID string) (map[string]string, error)
}

// The field keys, mirroring the integrations spec for ProviderTAK. Duplicated
// because this package does not import that one; a test pins them together.
const (
	takFieldHost       = "host"
	takFieldPort       = "port"
	takFieldServerName = "server_name"
	takFieldCA         = "ca_pem"
	takFieldClientCert = "client_cert_pem"
	takFieldClientKey  = "client_key_pem"
)

// externalCacheTTL is how long a resolved upstream is reused.
//
// The forwarder asks per position, and a kit reporting every few seconds would
// otherwise decrypt a credential row and parse a certificate chain each time. A
// minute is short enough that turning the feature on takes effect while somebody
// is still looking at the page.
const externalCacheTTL = time.Minute

// ExternalUpstreams resolves tenants' own TAK servers, with a short cache.
type ExternalUpstreams struct {
	src TAKConfigSource
	log *slog.Logger
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	cached map[string]*externalEntry
}

type externalEntry struct {
	// tenant is nil both when the tenant configured nothing and when what they
	// configured cannot be used; why distinguishes them.
	tenant *takfront.Tenant
	why    string
	at     time.Time
}

// NewExternalUpstreams wires one up. A nil source resolves nothing, which is what
// a Hub without the credential service wants.
func NewExternalUpstreams(src TAKConfigSource, log *slog.Logger) *ExternalUpstreams {
	if log == nil {
		log = slog.Default()
	}
	return &ExternalUpstreams{
		src: src, log: log, ttl: externalCacheTTL, now: time.Now,
		cached: map[string]*externalEntry{},
	}
}

// For returns the tenant's own upstream, or nil with a reason.
//
// A tenant who has configured nothing gets (nil, "") -- the ordinary case, and not
// a condition worth logging per position. A tenant whose settings cannot be used
// gets (nil, reason), so the reason can be surfaced once rather than swallowed.
func (e *ExternalUpstreams) For(ctx context.Context, tenantID string) (*takfront.Tenant, string) {
	if e == nil || e.src == nil || tenantID == "" {
		return nil, ""
	}

	e.mu.Lock()
	if c, ok := e.cached[tenantID]; ok && e.now().Sub(c.at) < e.ttl {
		t, why := c.tenant, c.why
		e.mu.Unlock()
		return t, why
	}
	e.mu.Unlock()

	values, err := e.src.TAKUpstream(ctx, tenantID)
	if err != nil {
		// Not cached: a store or decryption failure is transient, and caching it
		// would keep a tenant's traffic off their server for the whole TTL after a
		// blip. Nor is it logged per position -- the caller decides.
		return nil, "could not read this tenant's TAK settings: " + err.Error()
	}

	var (
		tenant *takfront.Tenant
		why    string
	)
	if len(values) > 0 {
		tenant, why = buildExternalTenant(tenantID, values)
		if why != "" {
			// Worth saying once per TTL rather than per position: somebody filled
			// the form in and it does not work, which they cannot see from here.
			e.log.Warn("takhosted: a tenant's own TAK server is configured but unusable",
				"tenant", tenantID, "reason", why)
		}
	}

	e.mu.Lock()
	e.cached[tenantID] = &externalEntry{tenant: tenant, why: why, at: e.now()}
	e.mu.Unlock()
	return tenant, why
}

// Forget drops a tenant's cached answer, for when their settings change.
func (e *ExternalUpstreams) Forget(tenantID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.cached, tenantID)
	e.mu.Unlock()
}

// externalLabel marks an upstream as a tenant's OWN TAK server rather than a
// hosted instance. A fixed string on purpose: it reaches logs and metrics, where
// a hosted instance's per-tenant random label must not, so the two are told apart
// by comparing against this rather than by guessing at the shape of the other.
const externalLabel = "external"

// buildExternalTenant turns stored values into a dialable upstream, or explains
// why it cannot.
//
// Every refusal names what is wrong in words a customer can act on, because these
// values came from a form they filled in and the only other place the failure
// would appear is a TLS error in a log they cannot read.
func buildExternalTenant(tenantID string, v map[string]string) (*takfront.Tenant, string) {
	host := strings.TrimSpace(v[takFieldHost])
	port := strings.TrimSpace(v[takFieldPort])
	if host == "" {
		return nil, "no host is set"
	}
	if port == "" {
		return nil, "no port is set"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return nil, "the port is not a number between 1 and 65535"
	}

	certPEM := strings.TrimSpace(v[takFieldClientCert])
	keyPEM := strings.TrimSpace(v[takFieldClientKey])
	caPEM := strings.TrimSpace(v[takFieldCA])
	if certPEM == "" || keyPEM == "" {
		return nil, "a client certificate and its key are both required: your server " +
			"authenticates this Hub with them"
	}
	if caPEM == "" {
		return nil, "your server's CA certificate is required: without it the Hub cannot " +
			"verify what it is connecting to"
	}

	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, "the client certificate and key do not load as a pair: " + err.Error()
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, "the CA does not parse as a PEM certificate"
	}

	return &takfront.Tenant{
		TenantID: tenantID,
		// Label identifies this upstream in logs and metrics only. "external"
		// rather than an instance label, because there is no instance: nothing
		// here was provisioned by the operator.
		Label:    externalLabel,
		Upstream: net.JoinHostPort(host, strconv.Itoa(n)),
		Identity: pair,
		// CA is deliberately left nil. It is the field takfront.NewDirectory
		// indexes tenants by, for identifying an inbound phone from its
		// certificate issuer -- and no phone arrives here. Setting it would imply
		// this upstream belongs in the front's directory, which it must not: two
		// tenants could perfectly well present the same CA for their own servers,
		// and the directory requires issuer uniqueness.
		CA:          nil,
		UpstreamCAs: pool,
		// Empty asks for the chain to be verified while the NAME check is skipped.
		// That is the right default for a server whose certificate names something
		// its address does not, which is most of them -- and the customer can set
		// it when their certificate does name the host.
		UpstreamServerName: strings.TrimSpace(v[takFieldServerName]),
	}, ""
}
