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
)

func init() {
	for _, reason := range []string{
		reasonServerFull, reasonHandshake, reasonUnauthorized, reasonTenantFull,
		reasonUpstream, reasonDeadline, reasonUnidentified,
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
// log only for decisions about an identified tenant.
//
// A refusal at the TLS layer is counted but NEVER audited. Audit entries are a
// per-tenant SHA-256 hash chain, so each one is a serialised database write;
// auditing unauthenticated connections would let anyone who can reach the port
// drive that chain, and there is no tenant to attribute the entry to anyway.
type recorder struct {
	audit Auditor
	log   *slog.Logger
}

// NewRecorder returns the Recorder to pass to NewServer. A nil Auditor keeps the
// metrics and drops the audit entries, which is what tests want.
func NewRecorder(a Auditor, log *slog.Logger) Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &recorder{audit: a, log: log}
}

func (r *recorder) Refused(ctx context.Context, tenantID, reason string, remote net.Addr) {
	streamsRefused.WithLabelValues(reason).Inc()
	// Only an identified tenant is audited, and only for a decision the tenant
	// could act on: a user whose certificate verified but who is no longer
	// allowed, or a tenant at its connection ceiling.
	if r.audit == nil || tenantID == "" {
		return
	}
	switch reason {
	case reasonUnauthorized, reasonTenantFull, reasonUpstream:
	default:
		return
	}
	if err := r.audit.Log(ctx, tenantID, actionStreamRefused, "tak-front",
		"refused: "+reason, host(remote)); err != nil {
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
