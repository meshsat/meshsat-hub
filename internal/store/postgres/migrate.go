package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// migration is one versioned, append-only schema step.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// migrationLockKey is the advisory lock that serialises Migrate across Hub
// replicas starting at the same time ("mesh" in ASCII).
const migrationLockKey = 0x6d657368

// ErrMigrationChecksum is returned when an applied migration's SQL no longer
// matches what was recorded: someone edited history instead of appending.
var ErrMigrationChecksum = errors.New("postgres: applied migration text changed")

func checksum(sqlText string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sqlText)))
	return hex.EncodeToString(sum[:])
}

// Migrate applies every pending migration in order inside one transaction per
// migration, under a transaction-scoped advisory lock so concurrent starters
// wait instead of racing. Applied versions are checksum-verified.
func (d *DB) Migrate(ctx context.Context) error {
	if err := validateMigrations(); err != nil {
		return err
	}
	if _, err := d.rawDB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("postgres: migration lock: %w", err)
	}

	want := checksum(m.SQL)
	var got string
	err = tx.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version = $1", m.Version).Scan(&got)
	switch {
	case err == nil:
		if got != want {
			return fmt.Errorf("%w: version %d (%s)", ErrMigrationChecksum, m.Version, m.Name)
		}
		return nil // already applied
	case errors.Is(err, sql.ErrNoRows):
		// pending
	default:
		return fmt.Errorf("postgres: read schema_migrations: %w", err)
	}

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("postgres: migration %d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
		m.Version, m.Name, want); err != nil {
		return fmt.Errorf("postgres: record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit migration %d: %w", m.Version, err)
	}
	slog.Info("postgres: migration applied", "version", m.Version, "name", m.Name)
	return nil
}

// validateMigrations enforces the append-only contract at build/test time:
// versions are contiguous from 1, names unique, SQL non-empty.
func validateMigrations() error {
	seen := map[string]bool{}
	for i, m := range migrations {
		if m.Version != i+1 {
			return fmt.Errorf("postgres: migrations must be contiguous from 1: index %d has version %d", i, m.Version)
		}
		if m.Name == "" || strings.TrimSpace(m.SQL) == "" {
			return fmt.Errorf("postgres: migration %d has an empty name or SQL", m.Version)
		}
		if seen[m.Name] {
			return fmt.Errorf("postgres: duplicate migration name %q", m.Name)
		}
		seen[m.Name] = true
	}
	return nil
}

// AppliedVersion returns the highest applied migration version (0 if none).
func (d *DB) AppliedVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := d.rawDB.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}
