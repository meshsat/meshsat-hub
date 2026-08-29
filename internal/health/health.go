package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// Probe is a function that checks the health of a dependency.
// It should return nil if healthy, or an error describing the failure.
type Probe func(ctx context.Context) error

// DetailedProbe returns structured health data in addition to pass/fail.
type DetailedProbe func(ctx context.Context) (map[string]any, error)

// Checker tracks the health of dependencies.
type Checker struct {
	mu             sync.RWMutex
	checks         map[string]bool
	probes         map[string]Probe
	detailedProbes map[string]DetailedProbe
	probeTimeout   time.Duration

	// Startup tracking: once all probes pass, startup is complete.
	startupMu   sync.RWMutex
	startupDone bool
}

// New creates a new health checker with the given probe timeout.
func New(probeTimeout time.Duration) *Checker {
	if probeTimeout <= 0 {
		probeTimeout = 3 * time.Second
	}
	return &Checker{
		checks:         make(map[string]bool),
		probes:         make(map[string]Probe),
		detailedProbes: make(map[string]DetailedProbe),
		probeTimeout:   probeTimeout,
	}
}

// Set updates the health status of a named dependency.
func (c *Checker) Set(name string, healthy bool) {
	c.mu.Lock()
	c.checks[name] = healthy
	c.mu.Unlock()
}

// AddProbe registers a named health probe that is called on each /readyz request.
// Probes are called with a short timeout to avoid blocking the endpoint.
func (c *Checker) AddProbe(name string, probe Probe) {
	c.mu.Lock()
	c.probes[name] = probe
	c.mu.Unlock()
}

// AddDetailedProbe registers a probe that returns structured data (e.g., Galera cluster info).
func (c *Checker) AddDetailedProbe(name string, probe DetailedProbe) {
	c.mu.Lock()
	c.detailedProbes[name] = probe
	c.mu.Unlock()
}

// CheckResult holds the status of a single health check.
type CheckResult struct {
	Status    string         `json:"status"`
	LatencyMS int64          `json:"latency_ms,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Response is the JSON structure returned by health endpoints.
type Response struct {
	Status string                  `json:"status"`
	Checks map[string]*CheckResult `json:"checks,omitempty"`
}

// LivezHandler always returns 200 if the process is running.
// @Summary      Liveness probe
// @Tags         health
// @Produce      json
// @Success      200  {object}  Response
// @Router       /healthz [get]
func LivezHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{Status: "ok"})
}

// ReadyzHandler returns 200 if all dependencies are healthy, 503 otherwise.
// @Summary      Readiness probe
// @Tags         health
// @Produce      json
// @Success      200  {object}  Response
// @Failure      503  {object}  Response
// @Router       /readyz [get]
func (c *Checker) ReadyzHandler(w http.ResponseWriter, _ *http.Request) {
	resp := c.evaluate()

	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// StartupzHandler returns 200 once all probes have passed at least once, 503 before that.
// @Summary      Startup probe
// @Tags         health
// @Produce      json
// @Success      200  {object}  Response
// @Failure      503  {object}  Response
// @Router       /startupz [get]
func (c *Checker) StartupzHandler(w http.ResponseWriter, _ *http.Request) {
	c.startupMu.RLock()
	done := c.startupDone
	c.startupMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if done {
		_ = json.NewEncoder(w).Encode(Response{Status: "ok"})
	} else {
		// Evaluate now and check if we just became ready.
		resp := c.evaluate()
		if resp.Status == "ok" {
			c.startupMu.Lock()
			c.startupDone = true
			c.startupMu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

func (c *Checker) evaluate() Response {
	c.mu.RLock()
	staticChecks := make(map[string]bool, len(c.checks))
	for k, v := range c.checks {
		staticChecks[k] = v
	}
	probes := make(map[string]Probe, len(c.probes))
	for k, v := range c.probes {
		probes[k] = v
	}
	detailed := make(map[string]DetailedProbe, len(c.detailedProbes))
	for k, v := range c.detailedProbes {
		detailed[k] = v
	}
	c.mu.RUnlock()

	resp := Response{
		Status: "ok",
		Checks: make(map[string]*CheckResult, len(staticChecks)+len(probes)+len(detailed)),
	}

	// Evaluate static checks.
	for name, healthy := range staticChecks {
		cr := &CheckResult{Status: "ok"}
		if !healthy {
			cr.Status = "unhealthy"
			resp.Status = "unhealthy"
		}
		resp.Checks[name] = cr
	}

	// Evaluate simple probes with timeout.
	if len(probes) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
		defer cancel()
		for name, probe := range probes {
			start := time.Now()
			err := probe(ctx)
			elapsed := time.Since(start)
			metrics.HealthProbeDuration.WithLabelValues(name).Observe(elapsed.Seconds())

			cr := &CheckResult{
				Status:    "ok",
				LatencyMS: elapsed.Milliseconds(),
			}
			if err != nil {
				cr.Status = "unhealthy"
				resp.Status = "unhealthy"
				if ctx.Err() != nil {
					metrics.HealthProbeTimeouts.WithLabelValues(name).Inc()
				}
			}
			resp.Checks[name] = cr
		}
	}

	// Evaluate detailed probes with timeout.
	if len(detailed) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
		defer cancel()
		for name, probe := range detailed {
			start := time.Now()
			detail, err := probe(ctx)
			elapsed := time.Since(start)
			metrics.HealthProbeDuration.WithLabelValues(name).Observe(elapsed.Seconds())

			cr := &CheckResult{
				Status:    "ok",
				LatencyMS: elapsed.Milliseconds(),
				Detail:    detail,
			}
			if err != nil {
				cr.Status = "unhealthy"
				resp.Status = "unhealthy"
				if ctx.Err() != nil {
					metrics.HealthProbeTimeouts.WithLabelValues(name).Inc()
				}
			}
			resp.Checks[name] = cr
		}
	}

	return resp
}
