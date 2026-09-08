package tenancy

import (
	"context"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// PurgeJob destroys the data of tenants whose grace period has expired.
//
// It follows the shape of the audit retention job: do the irreversible thing
// only after the reversible window has genuinely passed, log what was
// destroyed, and let a failure leave the tenant intact for the next run rather
// than half-erasing an account.
type PurgeJob struct {
	store store.Store
	audit *audit.Service
	grace time.Duration
	every time.Duration
	now   func() time.Time
}

// NewPurgeJob returns the job. grace of 0 uses store.PurgeGrace.
func NewPurgeJob(s store.Store, a *audit.Service, grace time.Duration) *PurgeJob {
	if grace <= 0 {
		grace = store.PurgeGrace
	}
	return &PurgeJob{store: s, audit: a, grace: grace, every: time.Hour, now: time.Now}
}

// Run purges until the context is cancelled. It is a leader singleton: two
// replicas racing to delete the same rows would be harmless but pointless, and
// the audit line should be written once.
func (j *PurgeJob) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	j.once(ctx) // a tenant whose grace expired while nobody was leader
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.once(ctx)
		}
	}
}

func (j *PurgeJob) once(ctx context.Context) {
	cutoff := j.now().UTC().Add(-j.grace)
	due, err := j.store.ListTenantsDeletedBefore(ctx, cutoff)
	if err != nil {
		slog.Error("purge: listing due tenants failed", "error", err)
		return
	}
	for _, t := range due {
		if t.ID == store.DefaultTenantID {
			continue
		}
		// The audit line goes in before the rows disappear: it is written to
		// the platform tenant, because the tenant it describes is about to
		// stop existing and its own chain goes with it.
		if j.audit != nil {
			detail := "tenant=" + t.ID + " slug=" + t.Slug + " deleted_at="
			if t.DeletedAt != nil {
				detail += t.DeletedAt.Format(time.RFC3339)
			}
			if err := j.audit.Log(ctx, store.DefaultTenantID, "tenant_purged", "purge_job", detail, ""); err != nil {
				slog.Warn("purge: audit failed, purging anyway", "tenant", t.ID, "error", err)
			}
		}
		if err := j.store.PurgeTenant(ctx, t.ID); err != nil {
			slog.Error("purge: failed, will retry next run", "tenant", t.ID, "error", err)
			continue
		}
		slog.Warn("purge: tenant data destroyed", "tenant", t.ID, "slug", t.Slug)
	}
}
