package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// RetentionConfig holds retention policy settings.
type RetentionConfig struct {
	RetentionDays int    // Days to keep entries (0 = disabled)
	ArchivePath   string // Path to write JSONL archives before purge (empty = no archive)
}

// RetentionStore defines the store methods needed by the retention service.
type RetentionStore interface {
	ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]store.AuditEntry, error)
	DeleteAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time) (int64, error)
}

// RunRetention starts a daily background goroutine that purges old audit entries.
// It blocks until ctx is cancelled.
func RunRetention(ctx context.Context, s RetentionStore, cfg RetentionConfig) {
	if cfg.RetentionDays <= 0 {
		slog.Info("audit: retention disabled")
		return
	}

	slog.Info("audit: retention enabled", "days", cfg.RetentionDays, "archive_path", cfg.ArchivePath)

	// Run once at startup, then daily.
	purge(ctx, s, cfg)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge(ctx, s, cfg)
		}
	}
}

func purge(ctx context.Context, s RetentionStore, cfg RetentionConfig) {
	cutoff := time.Now().UTC().AddDate(0, 0, -cfg.RetentionDays)
	tenantID := store.DefaultTenantID

	// Archive before purge if path is configured.
	if cfg.ArchivePath != "" {
		if err := archiveEntries(ctx, s, tenantID, cutoff, cfg.ArchivePath); err != nil {
			slog.Error("audit: archive failed, skipping purge", "error", err)
			return
		}
	}

	deleted, err := s.DeleteAuditEntriesBefore(ctx, tenantID, cutoff)
	if err != nil {
		slog.Error("audit: purge failed", "error", err)
		return
	}
	if deleted > 0 {
		metrics.AuditEntriesPurged.Add(float64(deleted))
		slog.Info("audit: purged entries", "count", deleted, "cutoff", cutoff.Format(time.DateOnly))
	}
}

func archiveEntries(ctx context.Context, s RetentionStore, tenantID string, before time.Time, dir string) error {
	entries, err := s.ListAuditEntriesBefore(ctx, tenantID, before, 0)
	if err != nil {
		return fmt.Errorf("list entries: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}

	filename := filepath.Join(dir, fmt.Sprintf("audit-%s.jsonl", before.Format("2006-01-02")))
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open archive file: %w", err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("write entry: %w", err)
		}
	}

	slog.Info("audit: archived entries", "count", len(entries), "file", filename)
	return nil
}
