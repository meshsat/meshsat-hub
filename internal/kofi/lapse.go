package kofi

import (
	"context"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// LapseJob returns tenants whose paid period has ended to the free tier.
//
// This is the only mechanism that ever downgrades anyone, because Ko-fi has no
// cancellation event to listen for. It runs hourly on the lease holder, the
// same way the purge job does, so the audit line is written once.
//
// What a lapse does and does not do is the whole point:
//
//   - It does not suspend the tenant, delete anything, or stop any traffic.
//   - Every registered device keeps reporting, keeps being routed, and keeps
//     its SOS path, whatever the plan says. The ceiling is checked when a
//     device is REGISTERED and nowhere else, so a tenant over the free limit
//     after a lapse simply cannot add a fifth device until they pay again.
//
// A subscription is not a reason to stop listening to somebody in the field.
type LapseJob struct {
	store  TenantStore
	audit  Auditor
	forget func(tenantID string)
	every  time.Duration
	now    func() time.Time
}

// NewLapseJob returns the job.
func NewLapseJob(s TenantStore, a Auditor, forget func(string)) *LapseJob {
	return &LapseJob{store: s, audit: a, forget: forget, every: time.Hour, now: time.Now}
}

// Run lapses expired plans until the context is cancelled.
func (j *LapseJob) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	j.Once(ctx) // catch plans that expired while nobody held the lease
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Once(ctx)
		}
	}
}

// Once performs a single pass. Exported so a test can drive it with a clock.
func (j *LapseJob) Once(ctx context.Context) {
	now := j.now().UTC()
	tenants, err := j.store.ListTenants(ctx)
	if err != nil {
		slog.Error("kofi: listing tenants for lapse failed", "error", err)
		return
	}
	for i := range tenants {
		t := tenants[i]
		if t.PlanExpiresAt == nil || t.PlanExpiresAt.After(now) {
			continue
		}
		if t.DeletedAt != nil || t.ID == store.DefaultTenantID {
			continue
		}
		if plans.Normalise(t.Plan) == plans.Free {
			// Already free; clear the stale date so this stops being looked at.
			t.PlanExpiresAt = nil
			if err := j.store.UpdateTenant(ctx, &t); err != nil {
				slog.Warn("kofi: could not clear a stale expiry", "tenant", t.ID, "error", err)
			}
			continue
		}
		was, expired := t.Plan, t.PlanExpiresAt.Format(time.RFC3339)
		t.Plan, t.PlanExpiresAt = plans.Free, nil
		if err := j.store.UpdateTenant(ctx, &t); err != nil {
			slog.Error("kofi: lapse failed, will retry next run", "tenant", t.ID, "error", err)
			continue
		}
		if j.forget != nil {
			j.forget(t.ID)
		}
		slog.Info("kofi: paid plan lapsed back to free; every registered device keeps working",
			"tenant", t.ID, "was", was, "expired", expired)
		if j.audit != nil {
			_ = j.audit.Log(ctx, t.ID, "subscription_lapsed", "lapse_job",
				"plan "+was+" -> "+plans.Free+", expired "+expired+
					" (existing devices unaffected; new registrations limited)", "")
		}
	}
}
