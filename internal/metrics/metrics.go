// Package metrics provides Prometheus instrumentation for MeshSat Hub.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTPRequestDuration tracks HTTP request latency by method, path, and status.
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "meshsat_hub_http_request_duration_seconds",
		Help:    "Duration of HTTP requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status_code"})

	// HTTPRequestsTotal counts HTTP requests by method, path, and status.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_http_requests_total",
		Help: "Total number of HTTP requests.",
	}, []string{"method", "path", "status_code"})

	// LeaderStatus is 1 while this instance holds leadership for singleton
	// services, 0 otherwise, labelled by elector backend.
	LeaderStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_leader",
		Help: "1 when this instance is the leader for singleton services, 0 otherwise.",
	}, []string{"backend"})

	// HTTPConnectionsActive tracks current in-flight HTTP requests.
	HTTPConnectionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_http_connections_active",
		Help: "Number of active HTTP connections being served.",
	})

	// MessageThroughput counts messages by direction and channel.
	MessageThroughput = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_message_throughput_total",
		Help: "Total messages processed by direction and channel.",
	}, []string{"direction", "channel"})

	// WSConnectionsActive tracks current WebSocket connections.
	WSConnectionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_ws_connections_active",
		Help: "Number of active WebSocket connections.",
	})

	// RelayPacketsTotal counts relay packet outcomes.
	RelayPacketsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_relay_packets_total",
		Help: "Total relay packets by result.",
	}, []string{"result"})

	// BuildInfo exposes version and configuration info as labels.
	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_info",
		Help: "Build and configuration info.",
	}, []string{"version", "mode", "go_version"})

	// RatelimitDecisions counts rate limit decisions (allowed/denied).
	RatelimitDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_ratelimit_decisions_total",
		Help: "Total rate limit decisions by result.",
	}, []string{"result"})

	// RatelimitViolations counts rate limit violations by type.
	RatelimitViolations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_ratelimit_violations_total",
		Help: "Total rate limit violations by type.",
	}, []string{"type"})

	// RatelimitOverridesActive tracks active rate limit overrides.
	RatelimitOverridesActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_ratelimit_overrides_active",
		Help: "Number of active rate limit overrides.",
	})

	// AuditEntriesPurged counts audit log entries removed by retention.
	AuditEntriesPurged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_audit_entries_purged_total",
		Help: "Total audit log entries purged by retention policy.",
	})

	// DependencyUp is 1 when the named dependency probe passed on the last
	// /readyz evaluation, 0 otherwise. Covers critical and informational probes.
	DependencyUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_dependency_up",
		Help: "1 when the dependency's last health probe passed, 0 otherwise.",
	}, []string{"dependency"})

	// HealthProbeTimeouts counts health probe timeout events.
	HealthProbeTimeouts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_health_probe_timeouts_total",
		Help: "Total health probe timeouts by probe name.",
	}, []string{"probe"})

	// HealthProbeDuration tracks health probe execution time.
	HealthProbeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "meshsat_hub_health_probe_duration_seconds",
		Help:    "Duration of health probe checks in seconds.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"probe"})

	// HeMB reassembly metrics (MESHSAT-489)

	// HeMBGenerationsDecoded counts successfully decoded HeMB generations.
	HeMBGenerationsDecoded = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_hemb_generations_decoded_total",
		Help: "Total HeMB generations successfully decoded.",
	}, []string{"bridge_id"})

	// HeMBSymbolsReceived counts individual RLNC-coded symbols received.
	HeMBSymbolsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_hemb_symbols_received_total",
		Help: "Total HeMB RLNC-coded symbols received.",
	}, []string{"bridge_id"})

	// HeMBActiveStreams tracks currently active reassembly streams.
	HeMBActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_hemb_active_streams",
		Help: "Number of currently active HeMB reassembly streams.",
	})

	// HeMBStaleStreamsPurged counts streams removed by the reap goroutine.
	HeMBStaleStreamsPurged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_hemb_stale_streams_purged_total",
		Help: "Total HeMB streams removed due to timeout.",
	})

	// HeMBReassemblyPending tracks pending (not yet decodable) generations.
	HeMBReassemblyPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_hemb_reassembly_pending",
		Help: "Number of pending HeMB generations awaiting more symbols.",
	})

	// HeMBBondGroupsTotal tracks configured bond groups per bridge.
	HeMBBondGroupsTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_hemb_bond_groups_total",
		Help: "Number of HeMB bond groups configured per bridge.",
	}, []string{"bridge_id"})

	// PaymentsUnattributedTotal counts subscription payments that matched no
	// tenant. Money arrived and nobody was upgraded: somebody has to look at
	// it, and until this existed the only trace was a log line (MESHSAT-1007).
	PaymentsUnattributedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_payments_unattributed_total",
		Help: "Payments that could not be attributed to a tenant and need a person.",
	})

	// PaymentsFailedTotal counts renewals the provider could not take. The plan
	// is deliberately NOT changed for one of these -- a failing card is the
	// provider retrying, not a cancellation -- so without this nothing at all
	// marks that a customer is on their way to lapsing (MESHSAT-1023).
	PaymentsFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_payments_failed_total",
		Help: "Subscription payments the provider could not take.",
	})

	// ReceiptsPending is how many receipts are waiting to be issued, and
	// ReceiptOldestPendingAge how long the oldest has waited. The outbox
	// deliberately never abandons a receipt -- it is a document somebody is owed
	// for money already taken -- so a wedged one retries quietly forever, and
	// until these existed nothing at all marked that a customer had paid and
	// received no VAT document (MESHSAT-1023).
	ReceiptsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_receipts_pending",
		Help: "Receipts recorded but not yet issued as a document.",
	})

	ReceiptOldestPendingAge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_receipt_oldest_pending_age_seconds",
		Help: "Age of the oldest receipt still waiting to be issued.",
	})

	// ReceiptsBlocked counts receipts parked for a person: an unknown currency,
	// a buyer outside the EU, an amount that would not parse. Money was taken
	// and the document cannot be issued without a decision. They sit at
	// GET /api/admin/receipts/blocked and nothing surfaced them before.
	ReceiptsBlocked = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_receipts_blocked",
		Help: "Receipts parked for a human decision before a document can be issued.",
	})

	// StripeFieldMissingTotal counts payload fields the Hub depends on that were
	// not where it looked. This exists because of how the first seven Stripe
	// defects presented: Stripe moved a field between API versions, the Go
	// struct kept the old json tag, encoding/json produced a zero value, and a
	// fallback made it look like normal operation -- no error, no log, no
	// failing test. Six separate bugs, one shape. Every fallback that stands in
	// for an absent field now increments this, so the next move is visible the
	// first time it happens rather than at the next audit (MESHSAT-1023).
	StripeFieldMissingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_stripe_field_missing_total",
		Help: "Stripe payload fields the Hub depends on that were absent, by field.",
	}, []string{"field"})

	// StripeAPIVersionTotal counts deliveries by the API version that rendered
	// them. The webhook endpoint's version is set in the Stripe dashboard,
	// entirely outside this repo, so this is the only record of what the Hub is
	// actually being sent. Low cardinality on purpose: one series per version.
	StripeAPIVersionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_stripe_api_version_total",
		Help: "Stripe webhook deliveries by the API version that rendered them.",
	}, []string{"version"})

	// DTN custody transfer metrics (MESHSAT-491)

	// CustodyAcceptedTotal counts custody offers accepted by the Hub.
	CustodyAcceptedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_custody_accepted_total",
		Help: "Total DTN custody offers accepted by the Hub.",
	})

	// CustodyExpiredTotal counts custody offers that expired without ACK.
	CustodyExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_custody_expired_total",
		Help: "Total DTN custody offers expired without acknowledgement.",
	})

	// CustodyPending tracks currently pending custody offers.
	CustodyPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_custody_pending",
		Help: "Number of pending DTN custody offers awaiting acknowledgement.",
	})
)

// SetBuildInfo sets the build info metric labels. Call once at startup.
func SetBuildInfo(version, mode, goVersion string) {
	BuildInfo.WithLabelValues(version, mode, goVersion).Set(1)
}

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}
