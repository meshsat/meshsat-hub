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

	// CloudloopMQTTConnected is 1 while a tenant's own Cloudloop MQTT feed is
	// connected on the leader (MESHSAT-1151). The platform feed from the
	// environment reports under the default tenant.
	CloudloopMQTTConnected = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_cloudloop_mqtt_connected",
		Help: "1 while the tenant's Cloudloop MQTT feed is connected, else 0.",
	}, []string{"tenant"})
	CloudloopMQTTMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_cloudloop_mqtt_messages_total",
		Help: "LingoMO messages received over a tenant's Cloudloop MQTT feed.",
	}, []string{"tenant"})
	CloudloopMQTTConnectFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_cloudloop_mqtt_connect_failures_total",
		Help: "Failed attempts to bring up a tenant's Cloudloop MQTT feed (bad certificate, unreachable broker).",
	}, []string{"tenant"})

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
	// The `kind` label separates the two very different things this counts.
	// "payment" is money that arrived and belongs to nobody -- somebody has been
	// charged and will get no receipt and no VAT document, which is worth waking
	// a person for. "lifecycle" is a subscription event naming a tenant this Hub
	// does not know: worth seeing, but no money moved and it is what a deleted
	// test tenant produces when Stripe sends a trailing event. Paging on both
	// made the first alert fire within minutes of being deployed, for a probe's
	// cleanup (MESHSAT-1023).
	PaymentsUnattributedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_payments_unattributed_total",
		Help: "Events that could not be attributed to a tenant, by whether money moved.",
	}, []string{"kind"})

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

	// WebhookUnexpectedSourceTotal counts inbound provider webhooks that arrived
	// from outside the expected source range. It NEVER gates anything: this is
	// the satellite MO path, the path an SOS arrives on, and a wrong allowlist
	// there is a distress message that never arrives. So the control is
	// observation only, and this counter is the whole of it (MESHSAT-995).
	WebhookUnexpectedSourceTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_webhook_unexpected_source_total",
		Help: "Provider webhooks processed from outside the expected source range, by provider.",
	}, []string{"provider"})

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

	// RelaySessions is the number of WebSocket relay sockets this replica
	// holds, by end (MESHSAT-612). Not the Reticulum relay: that is
	// meshsat_hub_relay_packets_total.
	RelaySessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meshsat_hub_relay_sessions",
		Help: "WebSocket relay sockets held by this replica, by role (bridge, client).",
	}, []string{"role"})

	// RelayFrames counts relay frames accepted from a socket, by direction
	// (up: client towards bridge, down: bridge towards client).
	RelayFrames = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_relay_frames_total",
		Help: "WebSocket relay frames accepted from a socket, by direction.",
	}, []string{"direction"})

	// RelayRejected counts relay connects and frames refused, by reason
	// (auth, tenant, budget, frame, envelope, upgrade).
	RelayRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_relay_rejected_total",
		Help: "WebSocket relay connects and frames refused, by reason.",
	}, []string{"reason"})
)

// SetBuildInfo sets the build info metric labels. Call once at startup.
func SetBuildInfo(version, mode, goVersion string) {
	BuildInfo.WithLabelValues(version, mode, goVersion).Set(1)
}

// AuthFailuresTotal counts rejected AUTHENTICATION attempts -- the 401s the
// auth middleware writes before routing, labelled by a stable reason and by
// the channel the request arrived on ("internet" or "onion").
//
// These were invisible. hubauth.Middleware is registered outside both
// metrics.ChiMiddleware and the request logger, and it short-circuits, so a
// rejected request incremented no counter, wrote no log line and left no audit
// row: credential stuffing against any /api/* route was a silent event, and
// the reasons were logged at Debug while production runs at info (MESHSAT-1190).
var AuthFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_auth_failures_total",
	Help: "Rejected authentication attempts, by reason and arrival channel.",
}, []string{"reason", "channel"})

// AuthzDenialsTotal counts rejected AUTHORISATION attempts -- the 403s from
// RequireRole, RequirePlatformAdmin and the tenant gate. Separate from
// AuthFailuresTotal on purpose: a spike in 401s is somebody trying to get in,
// a spike in 403s is somebody already inside reaching for something else, and
// the two want different responses.
var AuthzDenialsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_authz_denials_total",
	Help: "Rejected authorisation attempts, by requirement and arrival channel.",
}, []string{"requirement", "channel"})

// BreakGlassUseTotal counts uses of the static HUB_AUTH_TOKEN, which grants
// platform-admin over every tenant. Until MESHSAT-1195 the most privileged
// credential in the system was the only one whose use produced no metric, no
// log line and no audit row, so a leaked token was indistinguishable from the
// nightly verification job. "refused_onion" is a use rejected because it
// arrived on the hidden service, where there is no client identity to
// attribute it to.
var BreakGlassUseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_breakglass_use_total",
	Help: "Uses of the static platform-admin token, by outcome.",
}, []string{"outcome"})

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// A CounterVec has no series until a label combination is first used, and an
// absent series is not the same as a zero one: an alert written as
// increase(...{kind="payment"}[15m]) > 0 has nothing to evaluate against until
// the first unattributed payment ever happens. Materialise both values at
// startup so "no money has gone missing" is a fact the metric states rather
// than an absence somebody has to interpret (MESHSAT-1023).
func init() {
	for _, kind := range []string{"payment", "lifecycle"} {
		PaymentsUnattributedTotal.WithLabelValues(kind)
	}
	// Same reasoning for the security counters: an alert written as
	// increase(meshsat_hub_auth_failures_total[5m]) > N has nothing to
	// evaluate until the first failure of that exact kind ever happens, which
	// is precisely the moment you want the alert to already exist.
	// Verified empty in production on 2026-09-17: relay_rejected_total,
	// ratelimit_violations_total and webhook_unexpected_source_total had NO
	// series at all, so an alert on any of them could never have fired. The
	// webhook one is the sharper case -- its own comment says "the control is
	// observation only, and this counter is the whole of it", and the
	// observation was itself unobservable (MESHSAT-1190).
	for _, reason := range []string{"auth", "budget", "envelope", "frame", "tenant", "upgrade"} {
		RelayRejected.WithLabelValues(reason)
	}
	for _, kind := range []string{"daily_cap", "monthly_cap", "throttled"} {
		RatelimitViolations.WithLabelValues(kind)
	}
	for _, provider := range []string{"cloudloop", "globalstar", "rockblock", "sms", "stripe"} {
		WebhookUnexpectedSourceTotal.WithLabelValues(provider)
	}
	for _, outcome := range []string{"accepted", "refused_onion"} {
		BreakGlassUseTotal.WithLabelValues(outcome)
	}
	for _, ch := range []string{"internet", "onion"} {
		for _, reason := range []string{
			"missing_credential", "invalid_token", "invalid_claims",
			"unknown_subject", "invalid_api_key", "api_key_expired",
		} {
			AuthFailuresTotal.WithLabelValues(reason, ch)
		}
		for _, req := range []string{
			"viewer", "operator", "owner", "platform_admin",
			"tenant_required", "tenant_suspended", "tenant_deleted",
		} {
			AuthzDenialsTotal.WithLabelValues(req, ch)
		}
	}
}
