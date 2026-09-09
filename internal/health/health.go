package health

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// Probe is a function that checks the health of a dependency.
// It should return nil if healthy, or an error describing the failure.
type Probe func(ctx context.Context) error

// DetailedProbe returns structured health data in addition to pass/fail.
type DetailedProbe func(ctx context.Context) (map[string]any, error)

// Checker tracks the health of dependencies.
//
// Probes come in two classes. Critical probes (AddProbe, AddDetailedProbe,
// Set) decide the /readyz status: on Kubernetes a failing critical probe pulls
// the pod out of the Service. Informational probes (AddInfoProbe) are
// evaluated and exported as the meshsat_hub_dependency_up metric and shown by
// /readyz?verbose=1, but never change the status: a NATS, ntfy or hawkBit blip
// must not make the HTTP API unavailable.
type Checker struct {
	mu             sync.RWMutex
	checks         map[string]bool
	probes         map[string]Probe
	detailedProbes map[string]DetailedProbe
	infoProbes     map[string]Probe
	probeTimeout   time.Duration

	// Startup tracking: startup is complete once MarkStarted is called or all
	// critical probes have passed at least once.
	startupMu   sync.RWMutex
	startupDone bool

	// draining is set during graceful shutdown so /readyz returns 503 and the
	// load balancer stops sending new requests before the listener closes.
	draining atomic.Bool

	// diagToken gates ?verbose=1. The verbose response carries the raw error
	// string from every dependency probe -- Redis, NATS, ntfy, Apprise, hawkBit
	// -- which routinely names internal hosts, ports and TLS detail. Behind an
	// IP allowlist that was operator convenience; on a public endpoint it is
	// the richest unauthenticated disclosure the service has. Empty means
	// verbose is unavailable to everyone.
	diagToken string

	// cache holds the last non-verbose evaluation. /readyz and /startupz are
	// unauthenticated and each call fans out to every probe including a live
	// database query, so without this they are a request amplifier pointed at
	// our own backing services.
	cacheMu  sync.Mutex
	cached   *Response
	cachedAt time.Time
	cacheFor time.Duration
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
		infoProbes:     make(map[string]Probe),
		probeTimeout:   probeTimeout,
	}
}

// AddInfoProbe registers an informational probe. It is evaluated on every
// /readyz request (exported as meshsat_hub_dependency_up) and listed under
// "info" when the request carries ?verbose=1, but it never affects readiness.
func (c *Checker) AddInfoProbe(name string, probe Probe) {
	c.mu.Lock()
	c.infoProbes[name] = probe
	c.mu.Unlock()
}

// MarkStarted declares startup complete (migrations applied, listener up).
// After this /startupz answers 200 without evaluating probes.
func (c *Checker) MarkStarted() {
	c.startupMu.Lock()
	c.startupDone = true
	c.startupMu.Unlock()
}

// SetDraining makes /readyz answer 503 with status "draining" for the rest of
// the process lifetime. Call it first thing on SIGTERM.
func (c *Checker) SetDraining() {
	c.draining.Store(true)
}

// Draining reports whether SetDraining has been called.
func (c *Checker) Draining() bool {
	return c.draining.Load()
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
// Checks are the critical dependencies that decide Status; Info lists the
// informational dependencies and is only populated for verbose requests.
type Response struct {
	Status string                  `json:"status"`
	Checks map[string]*CheckResult `json:"checks,omitempty"`
	Info   map[string]*CheckResult `json:"info,omitempty"`
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

// ReadyzHandler returns 200 if all critical dependencies are healthy, 503
// otherwise (or while draining). Informational dependencies are included only
// with ?verbose=1.
// @Summary      Readiness probe
// @Tags         health
// @Produce      json
// @Param        verbose  query  string  false  "Include informational dependencies (1)"
// @Success      200  {object}  Response
// @Failure      503  {object}  Response
// @Router       /readyz [get]
func (c *Checker) ReadyzHandler(w http.ResponseWriter, r *http.Request) {
	verbose := r != nil && r.URL != nil && r.URL.Query().Get("verbose") == "1"
	if verbose && !c.diagAllowed(r) {
		// Silently downgrade rather than refuse: a probe that starts failing
		// because somebody added a query parameter is worse than a quiet one.
		verbose = false
	}

	var resp Response
	if verbose {
		resp = c.evaluate(true)
	} else {
		resp = c.cachedEvaluate()
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// diagAllowed reports whether this request may see dependency error detail.
func (c *Checker) diagAllowed(r *http.Request) bool {
	if c.diagToken == "" || r == nil {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(c.diagToken)) == 1
}

// SetDiagnosticsToken enables ?verbose=1 for callers presenting this bearer.
// The operator token is reused; there is no second secret to rotate.
func (c *Checker) SetDiagnosticsToken(token string) { c.diagToken = token }

// SetCacheFor bounds how long a readiness result is reused. 0 disables caching.
func (c *Checker) SetCacheFor(d time.Duration) { c.cacheFor = d }

// cachedEvaluate returns a recent non-verbose evaluation, running the probes
// only when the last result has aged out.
func (c *Checker) cachedEvaluate() Response {
	if c.cacheFor <= 0 {
		return c.evaluate(false)
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < c.cacheFor {
		return *c.cached
	}
	resp := c.evaluate(false)
	c.cached, c.cachedAt = &resp, time.Now()
	return resp
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
		// Evaluate now and check if we just became ready. Cached for the same
		// reason /readyz is: this path is unauthenticated and runs every probe.
		resp := c.cachedEvaluate()
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

func (c *Checker) evaluate(verbose bool) Response {
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
	info := make(map[string]Probe, len(c.infoProbes))
	for k, v := range c.infoProbes {
		info[k] = v
	}
	c.mu.RUnlock()

	resp := Response{
		Status: "ok",
		Checks: make(map[string]*CheckResult, len(staticChecks)+len(probes)+len(detailed)),
	}
	if c.draining.Load() {
		resp.Status = "draining"
	}

	// Evaluate static checks.
	for name, healthy := range staticChecks {
		cr := &CheckResult{Status: "ok"}
		if !healthy {
			cr.Status = "unhealthy"
			setUnhealthy(&resp)
		}
		metrics.DependencyUp.WithLabelValues(name).Set(boolGauge(healthy))
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
				setUnhealthy(&resp)
				if ctx.Err() != nil {
					metrics.HealthProbeTimeouts.WithLabelValues(name).Inc()
				}
			}
			metrics.DependencyUp.WithLabelValues(name).Set(boolGauge(err == nil))
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
				setUnhealthy(&resp)
				if ctx.Err() != nil {
					metrics.HealthProbeTimeouts.WithLabelValues(name).Inc()
				}
			}
			metrics.DependencyUp.WithLabelValues(name).Set(boolGauge(err == nil))
			resp.Checks[name] = cr
		}
	}

	// Evaluate informational probes: metric always, response only if verbose,
	// status never.
	if len(info) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
		defer cancel()
		if verbose {
			resp.Info = make(map[string]*CheckResult, len(info))
		}
		for name, probe := range info {
			start := time.Now()
			err := probe(ctx)
			elapsed := time.Since(start)
			metrics.HealthProbeDuration.WithLabelValues(name).Observe(elapsed.Seconds())
			metrics.DependencyUp.WithLabelValues(name).Set(boolGauge(err == nil))
			if err != nil && ctx.Err() != nil {
				metrics.HealthProbeTimeouts.WithLabelValues(name).Inc()
			}
			if verbose {
				cr := &CheckResult{Status: "ok", LatencyMS: elapsed.Milliseconds()}
				if err != nil {
					cr.Status = "unhealthy"
					cr.Detail = map[string]any{"error": err.Error()}
				}
				resp.Info[name] = cr
			}
		}
	}

	return resp
}

// setUnhealthy downgrades the response status without overriding "draining".
func setUnhealthy(resp *Response) {
	if resp.Status != "draining" {
		resp.Status = "unhealthy"
	}
}

func boolGauge(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}
