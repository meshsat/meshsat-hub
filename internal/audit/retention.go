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
	RetentionDays int       // Days to keep entries (0 = disabled)
	ArchivePath   string    // Directory for JSONL archives before purge (empty = no file archive)
	S3            *S3Config // Object store archive; takes precedence over ArchivePath when set
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

// purgeAll runs the archive+purge for every tenant; the default tenant is
// always included even when the tenants table is empty.
func purgeAll(ctx context.Context, s RetentionStore, cfg RetentionConfig, sink Sink) {
	cutoff := time.Now().UTC().AddDate(0, 0, -cfg.RetentionDays)
	tenants := []string{store.DefaultTenantID}
	if list, err := s.ListTenants(ctx); err != nil {
		slog.Warn("audit: list tenants failed, purging the default tenant only", "error", err)
	} else {
		for _, t := range list {
			if t.ID != store.DefaultTenantID {
				tenants = append(tenants, t.ID)
			}
		}
	}
	for _, tenantID := range tenants {
		purgeTenant(ctx, s, tenantID, cutoff, sink)
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
