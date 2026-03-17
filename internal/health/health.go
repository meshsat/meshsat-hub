package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Checker tracks the health of dependencies.
type Checker struct {
	mu     sync.RWMutex
	checks map[string]bool
}

// New creates a new health checker.
func New() *Checker {
	return &Checker{
		checks: make(map[string]bool),
	}
}

// Set updates the health status of a named dependency.
func (c *Checker) Set(name string, healthy bool) {
	c.mu.Lock()
	c.checks[name] = healthy
	c.mu.Unlock()
}

// Response is the JSON structure returned by health endpoints.
type Response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// LivezHandler always returns 200 if the process is running.
func LivezHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{Status: "ok"})
}

// ReadyzHandler returns 200 if all dependencies are healthy, 503 otherwise.
func (c *Checker) ReadyzHandler(w http.ResponseWriter, _ *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resp := Response{
		Status: "ok",
		Checks: make(map[string]string, len(c.checks)),
	}

	for name, healthy := range c.checks {
		if healthy {
			resp.Checks[name] = "ok"
		} else {
			resp.Checks[name] = "unhealthy"
			resp.Status = "unhealthy"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}
