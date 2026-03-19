package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Probe is a function that checks the health of a dependency.
// It should return nil if healthy, or an error describing the failure.
type Probe func(ctx context.Context) error

// Checker tracks the health of dependencies.
type Checker struct {
	mu     sync.RWMutex
	checks map[string]bool
	probes map[string]Probe
}

// New creates a new health checker.
func New() *Checker {
	return &Checker{
		checks: make(map[string]bool),
		probes: make(map[string]Probe),
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

// Response is the JSON structure returned by health endpoints.
type Response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
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
	c.mu.RLock()
	staticChecks := make(map[string]bool, len(c.checks))
	for k, v := range c.checks {
		staticChecks[k] = v
	}
	probes := make(map[string]Probe, len(c.probes))
	for k, v := range c.probes {
		probes[k] = v
	}
	c.mu.RUnlock()

	resp := Response{
		Status: "ok",
		Checks: make(map[string]string, len(staticChecks)+len(probes)),
	}

	// Evaluate static checks.
	for name, healthy := range staticChecks {
		if healthy {
			resp.Checks[name] = "ok"
		} else {
			resp.Checks[name] = "unhealthy"
			resp.Status = "unhealthy"
		}
	}

	// Evaluate active probes with a short timeout.
	if len(probes) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for name, probe := range probes {
			if err := probe(ctx); err != nil {
				resp.Checks[name] = "unhealthy"
				resp.Status = "unhealthy"
			} else {
				resp.Checks[name] = "ok"
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}
