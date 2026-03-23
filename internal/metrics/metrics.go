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
)

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}
