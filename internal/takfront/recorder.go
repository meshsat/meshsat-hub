package takfront

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics carry no tenant label, matching every other metric in the Hub: a
// label whose values are customer identifiers makes the series count grow with
// the customer count, and the audit log is where per-tenant detail belongs.
var (
	streamsOpened = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_takfront_streams_opened_total",
		Help: "Total number of TAK client streams accepted and proxied to a tenant server.",
	})
	streamsRefused = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_takfront_streams_refused_total",
		Help: "Total number of TAK client connections refused, by reason.",
	}, []string{"reason"})
	streamsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_takfront_streams_active",
		Help: "Number of TAK client streams currently proxied.",
	})
	streamBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_takfront_bytes_total",
		Help: "Bytes carried between TAK clients and tenant servers, by direction.",
	}, []string{"direction"})
	streamSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "meshsat_hub_takfront_stream_seconds",
		Help:    "How long proxied TAK client streams lasted.",
		Buckets: []float64{1, 10, 60, 300, 1800, 7200, 28800},
	})
)

// Refusal reasons, materialised below so that an alert on a rate can fire: an
// absent series is not a zero one.
const (
	reasonServerFull   = "server_full"
	reasonHandshake    = "handshake"
	reasonUnauthorized = "unauthorized"
	reasonTenantFull   = "tenant_full"
	reasonUpstream     = "upstream"
	reasonDeadline     = "deadline"
	reasonUnidentified = "unidentified"
	// reasonProbe is a connection that went away before it said anything. It is
	// counted but logged at Debug, because the overwhelming source of it is the
	// edge relay's own TCP health check (MESHSAT-1074).
	reasonProbe = "probe"
	// reasonPlaintext is a client that spoke something other than TLS to a TLS
	// port. Separated from reasonHandshake because the two need different
	// answers: a handshake refusal is a client we turned down and is worth a
	// WARN each time, while this is overwhelmingly unauthenticated noise on a
	// public port. The signal that matters is the RATE -- a customer whose TAK
	// client has TLS switched off is a sustained stream from one place, a
	// scanner is a single hit (MESHSAT-1074).
	reasonPlaintext = "plaintext"
)

func init() {
	for _, reason := range []string{
		reasonServerFull, reasonHandshake, reasonUnauthorized, reasonTenantFull,
		reasonUpstream, reasonDeadline, reasonUnidentified, reasonProbe,
		reasonPlaintext,
	} {
		streamsRefused.WithLabelValues(reason)
	}
	streamBytes.WithLabelValues("up")
	streamBytes.WithLabelValues("down")
}

// Auditor is the part of the audit service this package needs.
type Auditor interface {
	Log(ctx context.Context, tenantID, action, actor, detail, ip string) error
}

// Audit actions this package writes.
const (
	actionStreamOpened   = "tak_stream_opened"
	actionStreamRefused  = "tak_stream_refused"
	actionStreamFinished = "tak_stream_closed"
)

// recorder is the production Recorder: Prometheus for everything, and the audit
// log only for decisions that name a real client.
//
// Most refusals at the TLS layer are counted and NEVER audited. Audit entries
// are a per-tenant SHA-256 hash chain, so each one is a serialised database
// write; auditing unauthenticated connections would let anyone who can reach the
// port drive that chain, and there is no tenant to attribute the entry to
// anyway. Port 8089 faces the whole internet and is scanned constantly.
//
// reasonUnidentified is the one exception, and it is safe for a specific reason
// rather than by judgement (MESHSAT-1110). VerifyConnection refuses the
// handshake outright when ResolveChain fails, so an unknown issuer, an expired
// certificate or a missing client-auth usage never gets this far -- they are
// reasonHandshake, with no certificate we trusted. Reaching reasonUnidentified
// means the chain verified against a real tenant CA during the handshake and
// then failed on the re-read moments later: the directory changed underneath a
// connecting client, or a certificate crossed its expiry between the two checks.
// A scanner cannot manufacture that, and nothing else in the system would ever
// show it happened.
type recorder struct {
	audit Auditor
	// platformTenant owns the entries for events that have no tenant of their
	// own. Empty disables auditing those, which is what a caller that does not
	// want them passes.
	platformTenant string
	log            *slog.Logger
}

// NewRecorder returns the Recorder to pass to NewServer. A nil Auditor keeps the
// metrics and drops the audit entries, which is what tests want.
//
// platformTenant receives the entries for refusals that could not be attributed
// to a customer, the same way the purge job writes tenant_purged to the platform
// tenant because the tenant it describes has stopped existing.
func NewRecorder(a Auditor, platformTenant string, log *slog.Logger) Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &recorder{audit: a, platformTenant: platformTenant, log: log}
}

func (r *recorder) Refused(ctx context.Context, tenantID, reason string, remote net.Addr) {
	streamsRefused.WithLabelValues(reason).Inc()
	if r.audit == nil {
		return
	}
	owner := tenantID
	detail := "refused: " + reason
	switch reason {
	case reasonUnauthorized, reasonTenantFull, reasonUpstream:
		// An identified tenant, audited for a decision it could act on: a user
		// whose certificate verified but who is no longer allowed, or a tenant
		// at its connection ceiling.
		if tenantID == "" {
			return
		}
	case reasonUnidentified:
		// No tenant by construction -- being unable to name one IS the event.
		// See the type comment for why this one is not scanner-drivable.
		if r.platformTenant == "" {
			return
		}
		owner = r.platformTenant
		detail += " (a client that had just verified could no longer be attributed " +
			"to a tenant; the directory may have changed mid-handshake)"
	default:
		return
	}
	if err := r.audit.Log(ctx, owner, actionStreamRefused, "tak-front",
		detail, host(remote)); err != nil {
		r.log.Warn("takfront: audit refused", "error", err)
	}
}

func (r *recorder) Opened(ctx context.Context, p *Peer, remote net.Addr) {
	streamsOpened.Inc()
	streamsActive.Inc()
	if r.audit == nil {
		return
	}
	if err := r.audit.Log(ctx, p.Tenant.TenantID, actionStreamOpened, p.CommonName,
		"TAK client connected to "+p.Tenant.Label, host(remote)); err != nil {
		r.log.Warn("takfront: audit opened", "error", err)
	}
}

func (r *recorder) Closed(ctx context.Context, p *Peer, up, down int64, d time.Duration, cerr error) {
	streamsActive.Dec()
	streamBytes.WithLabelValues("up").Add(float64(up))
	streamBytes.WithLabelValues("down").Add(float64(down))
	streamSeconds.Observe(d.Seconds())
	if r.audit == nil {
		return
	}
	detail := "TAK client disconnected from " + p.Tenant.Label
	if cerr != nil {
		detail += ": " + cerr.Error()
	}
	if err := r.audit.Log(ctx, p.Tenant.TenantID, actionStreamFinished, p.CommonName,
		detail, host(remoteOrNil(ctx, p))); err != nil {
		r.log.Warn("takfront: audit closed", "error", err)
	}
}

// remoteOrNil exists only so Closed can share host(); the address is not
// carried into Closed because the connection is already gone by then.
func remoteOrNil(context.Context, *Peer) net.Addr { return nil }

func host(a net.Addr) string {
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}
