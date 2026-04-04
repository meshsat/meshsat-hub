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
)

// SetBuildInfo sets the build info metric labels. Call once at startup.
func SetBuildInfo(version, mode, goVersion string) {
	BuildInfo.WithLabelValues(version, mode, goVersion).Set(1)
}

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}
