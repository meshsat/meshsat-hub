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

	// takInstances destroys a tenant's hosted TAK server. nil when the Hub runs
	// without hosted TAK, which is the ordinary case.
	takInstances TAKInstanceDeleter
}

// TAKInstanceDeleter destroys a tenant's hosted OpenTAKServer (MESHSAT-1037).
//
// Declared here as a one-method interface rather than importing
// internal/takhosted, in the shape of internal/bridge.QuotaChecker and for a
// sharper reason: eight packages that carry field traffic import this one --
// rockblock, cloudloop, sms, routing, sos, position, bridge and message -- so an
// import of takhosted here would drag the Kubernetes custom-resource client and
// its in-cluster REST config onto every ingest path in the Hub. That is the same
// coupling that forces internal/quota's invariant test to scan source text
// instead of the dependency graph, and one instance of it is enough.
//
// Implemented by internal/takhosted and wired in from main.go.
type TAKInstanceDeleter interface {
	DeleteTAKInstanceForTenant(ctx context.Context, tenantID string) error
}

// NewPurgeJob returns the job. grace of 0 uses store.PurgeGrace.
func NewPurgeJob(s store.Store, a *audit.Service, grace time.Duration) *PurgeJob {
	if grace <= 0 {
		grace = store.PurgeGrace
	}
	return &PurgeJob{store: s, audit: a, grace: grace, every: time.Hour, now: time.Now}
}

// SetTAKInstances attaches the hosted TAK teardown. Without it a purge destroys
// a tenant's rows and leaves its OpenTAKServer running, because PurgeTenant
// reflects over tenant_id and a cluster object has no such column.
func (j *PurgeJob) SetTAKInstances(d TAKInstanceDeleter) { j.takInstances = d }

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
		// The tenant's own TAK server goes first, and a failure here stops this
		// tenant's purge entirely.
		//
		// Order matters twice over. PurgeTenant discovers its tables by reflecting
		// on tenant_id, so it already removes the tak_instances ROW -- but the
		// OpenTAKServer is a cluster object with no such column, and nothing would
		// ever come back for it. Destroying the rows first would leave a live
		// server holding a closed account's CoT history with no record in the Hub
		// that it exists: precisely the half-erasure this job promises not to
		// perform.
		//
		// And it runs BEFORE the audit line, not after, so a teardown that fails
		// does not leave "tenant_purged" written for a purge that did not happen
		// -- and written again on every later run.
		if j.takInstances != nil {
			if err := j.takInstances.DeleteTAKInstanceForTenant(ctx, t.ID); err != nil {
				slog.Error("purge: destroying the tenant's TAK server failed, will retry next run",
					"tenant", t.ID, "error", err)
				continue
			}
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
