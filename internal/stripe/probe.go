package stripe

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Probe answers "can this Hub take money right now?" for the health checker
// without putting Stripe on the readiness path.
//
// The public status page reports a Payments component from
// meshsat_hub_dependency_up{dependency="stripe"} (MESHSAT-1134). That gauge is
// refreshed on every /readyz call, and /readyz runs every ten seconds on every
// replica, so the probe must not make a Stripe call per evaluation. It makes
// one call per interval in its own goroutine and Health only reports the last
// result -- the same shape verifyStripeWebhookVersion uses at startup, kept
// running.
//
// It is registered with AddInfoProbe, never AddProbe: a Stripe outage must
// show on the status page, not take the Hub out of the Service. An ingest
// path cannot be gated by billing (internal/stripe/sos_invariant_test.go).
type Probe struct {
	c        *Client
	interval time.Duration

	mu      sync.Mutex
	err     error
	checked time.Time
}

// ErrNotCheckedYet is what Health reports before the first check has completed.
// It is an error deliberately: an unknown reads as 0 on the gauge, and the
// status page aggregates with max() across replicas, so a pod that has just
// started does not darken the component while the other pod knows better.
var ErrNotCheckedYet = errors.New("stripe: reachability not checked yet")

// ErrNoEnabledEndpoint means Stripe answered but has nowhere to deliver
// events: money can be taken and no plan granted, no receipt issued.
var ErrNoEnabledEndpoint = errors.New("stripe: this account has no enabled webhook endpoint")

// NewProbe builds a probe over c. interval <= 0 means hourly.
func NewProbe(c *Client, interval time.Duration) *Probe {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Probe{c: c, interval: interval, err: ErrNotCheckedYet}
}

// Start runs the first check immediately and then every interval until ctx
// ends. Each check has its own timeout so a hung Stripe cannot wedge the loop.
func (p *Probe) Start(ctx context.Context) {
	go func() {
		_ = p.Check(ctx)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = p.Check(ctx)
			}
		}
	}()
}

// Check performs one reachability check and records the result.
func (p *Probe) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := p.check(cctx)
	p.mu.Lock()
	p.err, p.checked = err, time.Now()
	p.mu.Unlock()
	return err
}

func (p *Probe) check(ctx context.Context) error {
	eps, err := p.c.WebhookAPIVersion(ctx)
	if err != nil {
		return err
	}
	for _, ep := range eps {
		if ep.Enabled() {
			return nil
		}
	}
	return ErrNoEnabledEndpoint
}

// Health is the health.Probe: it returns the last recorded result and never
// touches the network, so /readyz stays cheap.
func (p *Probe) Health(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// LastChecked reports when the recorded result was produced (zero before the
// first check).
func (p *Probe) LastChecked() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checked
}
