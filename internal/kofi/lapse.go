package kofi

import (
	"context"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"log/slog"
	"strings"
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
	// mail warns a customer before their plan ends and tells them when it has.
	// Nothing used to: a lapsed customer found out when a registration was
	// refused. nil means no relay is configured and the lapse still happens.
	mail       mail.Sender
	users      UserLookup
	hubURL     string
	upgradeURL string
	// warnAt is how long before expiry the warning goes out.
	warnAt time.Duration
}

// SetMailer turns on the lapse warning and the lapsed notice.
func (j *LapseJob) SetMailer(s mail.Sender, u UserLookup, hubURL, upgradeURL string) {
	j.mail, j.users, j.hubURL, j.upgradeURL = s, u, hubURL, upgradeURL
	if j.warnAt == 0 {
		j.warnAt = 3 * 24 * time.Hour
	}
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
		if t.PlanExpiresAt == nil {
			continue
		}
		if t.PlanExpiresAt.After(now) {
			// Not expired yet. Warn once inside the window, while the customer
			// can still act on it.
			j.warn(ctx, &t, now)
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
		was, endedAt := t.Plan, *t.PlanExpiresAt
		expired := endedAt.Format(time.RFC3339)
		t.Plan, t.PlanExpiresAt = plans.Free, nil
		if err := j.store.UpdateTenant(ctx, &t); err != nil {
			slog.Error("kofi: lapse failed, will retry next run", "tenant", t.ID, "error", err)
			continue
		}
		if j.forget != nil {
			j.forget(t.ID)
		}
		if j.mail != nil {
			if to := j.ownerEmail(ctx, &t); to != "" {
				subject, body := mail.Lapsed(j.ownerName(ctx, &t), was, endedAt, j.upgradeURL)
				mail.SendOrLog(ctx, j.mail, to, subject, body, "plan lapsed")
			}
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

// warn sends the pre-lapse notice once, inside the warning window. It is driven
// off warned_at on the tenant so an hourly job does not mail every hour.
func (j *LapseJob) warn(ctx context.Context, t *store.Tenant, now time.Time) {
	if j.mail == nil || j.warnAt <= 0 || t.PlanExpiresAt == nil {
		return
	}
	if plans.Normalise(t.Plan) == plans.Free {
		return
	}
	if t.DeletedAt != nil || t.ID == store.DefaultTenantID {
		return
	}
	if t.PlanExpiresAt.Sub(now) > j.warnAt {
		return // too early
	}
	if t.LapseWarnedAt != nil && !t.LapseWarnedAt.Before(*t.PlanExpiresAt) {
		return // already warned for this expiry
	}
	to := j.ownerEmail(ctx, t)
	if to == "" {
		return
	}
	subject, body := mail.LapseWarning(j.ownerName(ctx, t), t.Plan, *t.PlanExpiresAt, j.upgradeURL, t.KofiClaimCode)
	mail.SendOrLog(ctx, j.mail, to, subject, body, "lapse warning")
	warned := now
	t.LapseWarnedAt = &warned
	if err := j.store.UpdateTenant(ctx, t); err != nil {
		// Not fatal: the worst case is a second warning next hour.
		slog.Warn("kofi: could not record that a lapse warning was sent", "tenant", t.ID, "error", err)
	}
}

func (j *LapseJob) ownerEmail(ctx context.Context, t *store.Tenant) string {
	if j.users == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := j.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(u.Email))
}

func (j *LapseJob) ownerName(ctx context.Context, t *store.Tenant) string {
	if j.users == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := j.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return u.Name
}
