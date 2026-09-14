package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// RetentionConfig holds retention policy settings.
type RetentionConfig struct {
	RetentionDays int // Days to keep entries (0 = disabled); the platform DEFAULT,
	// used by any tenant that has not chosen its own.
	// MinDays and MaxDays bound a tenant's own choice (0 = unbounded). The
	// floor is the load-bearing one: without it a tenant could set a retention
	// short enough that the evidence of a security event in their own account
	// is gone before anyone looks at it.
	MinDays     int
	MaxDays     int
	ArchivePath string    // Directory for JSONL archives before purge (empty = no file archive)
	S3          *S3Config // Object store archive; takes precedence over ArchivePath when set
}

// RetentionStore defines the store methods needed by the retention service.
type RetentionStore interface {
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]store.AuditEntry, error)
	DeleteAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time) (int64, error)
}

// sinkFor builds the archive sink from the configuration (nil = purge only).
func sinkFor(cfg RetentionConfig) (Sink, error) {
	if cfg.S3 != nil {
		return NewS3Sink(*cfg.S3)
	}
	if cfg.ArchivePath != "" {
		return FileSink{Dir: cfg.ArchivePath}, nil
	}
	return nil, nil
}

// RunRetention starts a daily background goroutine that archives and purges
// old audit entries of every tenant. It blocks until ctx is cancelled.
func RunRetention(ctx context.Context, s RetentionStore, cfg RetentionConfig) {
	if cfg.RetentionDays <= 0 {
		slog.Info("audit: retention disabled")
		return
	}
	sink, err := sinkFor(cfg)
	if err != nil {
		slog.Error("audit: retention disabled, archive sink misconfigured", "error", err)
		return
	}
	name := "none"
	if sink != nil {
		name = sink.Name()
	}
	slog.Info("audit: retention enabled", "days", cfg.RetentionDays, "archive", name)

	// Run once at startup, then daily.
	purgeAll(ctx, s, cfg, sink)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purgeAll(ctx, s, cfg, sink)
		}
	}
}

// purgeAll runs the archive+purge for every tenant, EACH AGAINST ITS OWN
// CUTOFF; the default tenant is always included even when the tenants table is
// empty.
//
// MESHSAT-1117: the cutoff used to be computed once, from
// HUB_AUDIT_RETENTION_DAYS, and applied to everyone. A tenant's audit log is
// its own record of who did what in its account, and how long it needs keeping
// is that customer's compliance question. A tenant that has chosen nothing
// (0) still gets the platform default, which is every tenant until an owner
// says otherwise.
func purgeAll(ctx context.Context, s RetentionStore, cfg RetentionConfig, sink Sink) {
	now := time.Now().UTC()
	// days resolves one tenant's retention: its own choice, or the platform
	// default. A stored value is already bounded by the API; this clamps again
	// because a row can also be written by hand, and a zero or negative cutoff
	// here would purge the tenant's entire audit log on the next tick.
	days := func(own int) int {
		if own <= 0 {
			return cfg.RetentionDays
		}
		if cfg.MinDays > 0 && own < cfg.MinDays {
			return cfg.MinDays
		}
		if cfg.MaxDays > 0 && own > cfg.MaxDays {
			return cfg.MaxDays
		}
		return own
	}

	type target struct {
		id   string
		days int
	}
	targets := []target{{store.DefaultTenantID, cfg.RetentionDays}}
	if list, err := s.ListTenants(ctx); err != nil {
		slog.Warn("audit: list tenants failed, purging the default tenant only", "error", err)
	} else {
		for _, t := range list {
			if t.ID == store.DefaultTenantID {
				// The default tenant is already in the list, but it is a real
				// tenant with a real row and may have chosen its own retention.
				targets[0].days = days(t.AuditRetentionDays)
				continue
			}
			targets = append(targets, target{t.ID, days(t.AuditRetentionDays)})
		}
	}
	for _, tt := range targets {
		if tt.days <= 0 {
			continue // retention disabled for this tenant
		}
		purgeTenant(ctx, s, tt.id, now.AddDate(0, 0, -tt.days), sink)
	}
}

func purgeTenant(ctx context.Context, s RetentionStore, tenantID string, cutoff time.Time, sink Sink) {
	if sink != nil {
		entries, err := s.ListAuditEntriesBefore(ctx, tenantID, cutoff, 0)
		if err != nil {
			slog.Error("audit: list entries for archive failed, skipping purge", "tenant", tenantID, "error", err)
			return
		}
		if len(entries) > 0 {
			if err := sink.Write(ctx, tenantID, cutoff, entries); err != nil {
				slog.Error("audit: archive failed, skipping purge", "tenant", tenantID, "sink", sink.Name(), "error", err)
				return
			}
		}
	}
	deleted, err := s.DeleteAuditEntriesBefore(ctx, tenantID, cutoff)
	if err != nil {
		slog.Error("audit: purge failed", "tenant", tenantID, "error", err)
		return
	}
	if deleted > 0 {
		metrics.AuditEntriesPurged.Add(float64(deleted))
		slog.Info("audit: purged entries", "tenant", tenantID, "count", deleted, "cutoff", cutoff.Format(time.DateOnly))
	}
}
