// Package observability provides error tracking and categorization.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Error categories for classification.
const (
	ErrTimeout     = "timeout"
	ErrAuth        = "auth"
	ErrValidation  = "validation"
	ErrDatabase    = "database"
	ErrMQTT        = "mqtt"
	ErrExternalAPI = "external_api"
	ErrInternal    = "internal"
)

var errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_errors_total",
	Help: "Total errors by category and component.",
}, []string{"category", "component"})

// RecordError increments the error counter for the given category and component.
func RecordError(category, component string) {
	errorsTotal.WithLabelValues(category, component).Inc()
}
