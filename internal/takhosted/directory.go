package takhosted

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// DirectoryRefresher keeps the front's tenant snapshot current.
//
// It runs on EVERY replica, not as a leader singleton. The front is a door:
// every replica accepts phones, so every replica needs to resolve any tenant's
// certificate issuer. A leader-only refresher would leave the other replicas
// serving a snapshot that never changed.
//
// # Two failure modes this is shaped around
//
// takfront.NewDirectory is all-or-nothing: it returns an error on the FIRST
// tenant missing an id, CA, upstream, identity or upstream CA pool. So a single
// tenant whose instance is still provisioning would reject the whole directory
// and take every other tenant's phones down with it. Tenants are therefore
// FILTERED before construction, and a tenant that is not ready yet is skipped
// with a log line rather than allowed to fail the batch.
//
// And takfront.Server.Serve dereferences the stored directory on its first line,
// while SetDirectory(nil) is a silent no-op. An empty snapshot must therefore be
// published BEFORE Serve runs, which is the normal cold-start state on a cluster
// with no TAK tenants yet.
type DirectoryRefresher struct {
	client *Client
	certs  *IdentityKeeper
	log    *slog.Logger

	// setDirectory is takfront.Server.SetDirectory. Injected rather than holding
	// the server, so this is testable without a listener.
	setDirectory func(*takfront.Directory)

	mu   sync.Mutex
	last *takfront.Directory
	// skipped remembers why each tenant was left out, for the readiness probe
	// and for a person asking "where is my TAK server".
	skipped map[string]string
	// tenants is the same set the published Directory holds, keyed by tenant id.
	//
	// It exists because takfront.Directory is deliberately immutable and indexed
	// by CA SUBJECT, not by tenant id -- it has no accessor to enumerate tenants
	// or look one up, and adding one would invite mutating a snapshot that
	// handshakes read without locking. The outbound forwarder needs a *Tenant to
	// dial, so it reads it from here: one source of truth, built in the same pass
	// that publishes the directory.
	tenants map[string]takfront.Tenant
}

// DefaultRefreshInterval is how often the snapshot is rebuilt. Five minutes is
// the plan's figure and is a compromise: a new tenant waits up to that long for
// its first phone to connect, and a suspended tenant keeps being served for up to
// that long -- which is why suspension also goes through the authorizer, where it
// takes effect within the authorizer's much shorter TTL.
const DefaultRefreshInterval = 5 * time.Minute

// NewDirectoryRefresher wires one up. setDirectory is normally srv.SetDirectory.
func NewDirectoryRefresher(c *Client, k *IdentityKeeper,
	setDirectory func(*takfront.Directory), log *slog.Logger) *DirectoryRefresher {
	if log == nil {
		log = slog.Default()
	}
	return &DirectoryRefresher{
		client: c, certs: k,
		setDirectory: setDirectory, log: log,
		skipped: map[string]string{},
		tenants: map[string]takfront.Tenant{},
	}
}

// PublishEmpty installs an empty snapshot.
//
// Called before Serve, always. takfront.Server.Serve reads dir.Load().Len() on
// its first line and SetDirectory(nil) does nothing, so a front started without
// this would panic on a nil directory the moment it began listening -- and a
// cluster with no TAK tenants yet is the ordinary case, not an edge one.
func (r *DirectoryRefresher) PublishEmpty() error {
	d, err := takfront.NewDirectory(nil)
	if err != nil {
		return fmt.Errorf("takhosted: empty directory: %w", err)
	}
	r.mu.Lock()
	r.last = d
	r.tenants = map[string]takfront.Tenant{}
	r.mu.Unlock()
	r.setDirectory(d)
	return nil
}

// Run refreshes until ctx is done. Not a leader singleton: see the type comment.
func (r *DirectoryRefresher) Run(ctx context.Context) {
	r.runEvery(ctx, DefaultRefreshInterval)
}

func (r *DirectoryRefresher) runEvery(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultRefreshInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	if err := r.Refresh(ctx); err != nil {
		r.log.Warn("takhosted: first directory refresh failed, serving what we have", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Refresh(ctx); err != nil {
				r.log.Warn("takhosted: directory refresh failed, keeping the previous snapshot", "error", err)
			}
		}
	}
}

// Refresh rebuilds the snapshot once.
//
// On any failure the PREVIOUS snapshot stays published. Replacing a working
// directory with nothing because the API server was briefly unreachable would
// drop every phone in every tenant, which is a far worse outcome than serving a
// few minutes of slightly stale tenant set.
func (r *DirectoryRefresher) Refresh(ctx context.Context) error {
	instances, err := r.client.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	tenants := make([]takfront.Tenant, 0, len(instances))
	skipped := map[string]string{}

	for i := range instances {
		inst := instances[i]
		tenantID := inst.Spec.TenantID
		if tenantID == "" {
			r.log.Warn("takhosted: instance with no tenant id, skipped", "name", inst.Metadata.Name)
			continue
		}
		t, why := r.tenantFor(ctx, inst)
		if why != "" {
			skipped[tenantID] = why
			continue
		}
		tenants = append(tenants, *t)
	}

	d, err := takfront.NewDirectory(tenants)
	if err != nil {
		// A duplicate CA subject lands here. It is a real defect -- two tenants
		// cannot share an issuer, because the issuer is the only thing naming the
		// tenant -- so the previous snapshot is kept and the error is loud.
		return fmt.Errorf("build directory from %d tenants: %w", len(tenants), err)
	}

	byID := make(map[string]takfront.Tenant, len(tenants))
	for _, t := range tenants {
		byID[t.TenantID] = t
	}

	r.mu.Lock()
	r.last = d
	r.skipped = skipped
	r.tenants = byID
	r.mu.Unlock()
	r.setDirectory(d)

	r.log.Info("takhosted: directory refreshed", "tenants", d.Len(), "skipped", len(skipped))
	return nil
}

// tenantFor turns one instance into a takfront.Tenant, or explains why it cannot
// yet. The reason string is for a human: it ends up in a log line and in the
// readiness detail.
func (r *DirectoryRefresher) tenantFor(ctx context.Context, inst TakInstance) (*takfront.Tenant, string) {
	if inst.Status.Phase != PhaseReady {
		return nil, "instance phase is " + orNone(inst.Status.Phase)
	}
	if inst.Status.Host == "" {
		return nil, "instance reports no host yet"
	}
	if inst.Status.CACertPEM == "" {
		return nil, "instance reports no CA certificate yet"
	}
	ca, err := parseOneCert(inst.Status.CACertPEM)
	if err != nil {
		// Not transient: a malformed CA will not fix itself, and every phone in
		// this tenant depends on it, so it is worth saying plainly.
		r.log.Error("takhosted: tenant CA does not parse",
			"tenant", inst.Spec.TenantID, "error", err)
		return nil, "tenant CA does not parse: " + err.Error()
	}

	// The Hub's own certificate for THIS tenant, signed by THIS tenant's CA. The
	// keeper obtains it by asking the operator and caches it; a tenant whose
	// certificate is not issued yet is skipped rather than failing the batch.
	identity, err := r.certs.For(ctx, inst.Spec.TenantID, inst.Spec.Label)
	if err != nil {
		if errors.Is(err, ErrIdentityPending) {
			return nil, "waiting for the Hub's upstream certificate to be issued"
		}
		r.log.Warn("takhosted: no upstream identity for tenant",
			"tenant", inst.Spec.TenantID, "error", err)
		return nil, "upstream identity unavailable: " + err.Error()
	}

	// The upstream's server certificate is signed by the same tenant CA, so the
	// tenant's own CA is the pool that verifies it.
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	return &takfront.Tenant{
		TenantID:    inst.Spec.TenantID,
		Label:       inst.Spec.Label,
		CA:          ca,
		Upstream:    inst.Status.Host,
		Identity:    identity,
		UpstreamCAs: pool,
		// Left empty deliberately: OpenTAKServer's own server certificate carries
		// names that no in-cluster address matches, so takfront verifies the
		// chain against UpstreamCAs and skips the name check. Setting a name here
		// would fail every upstream dial.
		UpstreamServerName: "",
	}, ""
}

// Snapshot reports the current tenant count and the tenants left out, for the
// readiness probe and for answering "why is my TAK server not reachable".
func (r *DirectoryRefresher) Snapshot() (tenants int, skipped map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.skipped))
	for k, v := range r.skipped {
		out[k] = v
	}
	if r.last == nil {
		return 0, out
	}
	return r.last.Len(), out
}

// parseOneCert decodes the first CERTIFICATE block in a PEM bundle.
func parseOneCert(pemText string) (*x509.Certificate, error) {
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no CERTIFICATE block found")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// loadPair builds a tls.Certificate from PEM, with the key held only in memory.
func loadPair(certPEM, keyPEM []byte) (tls.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("takhosted: upstream identity does not load: %w", err)
	}
	return pair, nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}
