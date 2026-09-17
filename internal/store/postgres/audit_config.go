package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Audit log, device config versioning and system config. Semantics mirror
// internal/store/mariadb (same generated IDs, same ordering and limits, same
// tenant scoping, not-found = sql.ErrNoRows).

// --- Audit log ---

const auditColumns = "id, action, actor, detail, ip, prev_hash, hash, hash_version, created_at"

// latestAuditEntrySQL is the chain's notion of "latest": created_at is written
// monotonic per tenant by the audit service, so this is unambiguous.
const latestAuditEntrySQL = "SELECT " + auditColumns + " FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1"

func scanAuditEntry(sc interface{ Scan(...any) error }, a *store.AuditEntry) error {
	if err := sc.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.HashVersion, &a.CreatedAt); err != nil {
		return err
	}
	a.CreatedAt = utc(a.CreatedAt)
	return nil
}

func (d *DB) InsertAuditEntry(ctx context.Context, tenantID string, a *store.AuditEntry) error {
	return insertAuditEntry(ctx, d.db, tenantID, a)
}

// insertAuditEntry writes one row on whatever handle it is given (the pool,
// or the transaction AppendAuditEntry holds the tenant lock in).
func insertAuditEntry(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, tenantID string, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	_, err := x.ExecContext(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip, prev_hash, hash, hash_version, created_at, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP, a.PrevHash, a.Hash, a.HashVersion, a.CreatedAt.UTC(), tenantID)
	return err
}

// AppendAuditEntry chains one entry onto a tenant's log under a transaction-
// scoped advisory lock keyed by the tenant, so the two Hub replicas append in
// turn instead of both reading the same latest row. The lock is released with
// the transaction, committed or not; there is nothing to leak.
func (d *DB) AppendAuditEntry(ctx context.Context, tenantID string, build func(prev *store.AuditEntry) (*store.AuditEntry, error)) error {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audit append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "audit:"+tenantID); err != nil {
		return fmt.Errorf("lock audit chain: %w", err)
	}
	var prev *store.AuditEntry
	var latest store.AuditEntry
	switch err := scanAuditEntry(tx.QueryRowContext(ctx, latestAuditEntrySQL, tenantID), &latest); {
	case err == nil:
		prev = &latest
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("get latest audit entry: %w", err)
	}
	entry, err := build(prev)
	if err != nil {
		return err
	}
	if err := insertAuditEntry(ctx, tx, tenantID, entry); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]store.AuditEntry, error) {
	// limit <= 0 means every entry: the audit chain verifier asks for the whole
	// chain this way (a literal LIMIT 0 verified nothing and reported "valid").
	q := "SELECT " + auditColumns + " FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC"
	args := []any{tenantID}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := scanAuditEntry(rows, &a); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, rows.Err()
}

func (d *DB) GetLatestAuditEntry(ctx context.Context, tenantID string) (*store.AuditEntry, error) {
	var a store.AuditEntry
	row := d.db.QueryRowContext(ctx, latestAuditEntrySQL, tenantID)
	if err := scanAuditEntry(row, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]store.AuditEntry, error) {
	q := "SELECT " + auditColumns + " FROM audit_log WHERE tenant_id=$1 AND created_at < $2"
	args := []any{tenantID, before.UTC()}
	if limit > 0 {
		args = append(args, limit)
		q += " LIMIT $" + strconv.Itoa(len(args))
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := scanAuditEntry(rows, &a); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, rows.Err()
}

func (d *DB) DeleteAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		"DELETE FROM audit_log WHERE tenant_id=$1 AND created_at < $2",
		tenantID, before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Device config versioning ---

const deviceConfigColumns = "id, device_imei, version, config, author, comment, created_at"

func scanDeviceConfig(sc interface{ Scan(...any) error }, c *store.DeviceConfig) error {
	if err := sc.Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt); err != nil {
		return err
	}
	c.CreatedAt = utc(c.CreatedAt)
	return nil
}

// CreateDeviceConfig stores a new snapshot as version max(version)+1 for the
// device within the tenant and sets c.Version accordingly.
func (d *DB) CreateDeviceConfig(ctx context.Context, tenantID string, c *store.DeviceConfig) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("cfg-%d", time.Now().UnixNano())
	}
	var maxVersion int
	if err := d.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM device_configs WHERE device_imei=$1 AND tenant_id=$2",
		c.DeviceIMEI, tenantID,
	).Scan(&maxVersion); err != nil {
		return err
	}
	c.Version = maxVersion + 1

	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_configs (id, device_imei, version, config, author, comment, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		c.ID, c.DeviceIMEI, c.Version, c.Config, c.Author, c.Comment, tenantID)
	return err
}

func (d *DB) GetDeviceConfigLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	row := d.db.QueryRowContext(ctx,
		"SELECT "+deviceConfigColumns+" FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 ORDER BY version DESC LIMIT 1",
		deviceIMEI, tenantID)
	if err := scanDeviceConfig(row, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) GetDeviceConfigVersion(ctx context.Context, tenantID string, deviceIMEI string, version int) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	row := d.db.QueryRowContext(ctx,
		"SELECT "+deviceConfigColumns+" FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 AND version=$3",
		deviceIMEI, tenantID, version)
	if err := scanDeviceConfig(row, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListDeviceConfigVersions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.DeviceConfig, error) {
	query := "SELECT " + deviceConfigColumns + " FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 ORDER BY version DESC"
	args := []any{deviceIMEI, tenantID}
	if limit > 0 {
		args = append(args, limit)
		query += " LIMIT $" + strconv.Itoa(len(args))
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var configs []store.DeviceConfig
	for rows.Next() {
		var c store.DeviceConfig
		if err := scanDeviceConfig(rows, &c); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

// --- System config ---

// GetSystemConfig returns the value for key; a missing key is sql.ErrNoRows.
func (d *DB) GetSystemConfig(ctx context.Context, key string) (string, error) {
	var value string
	if err := d.db.QueryRowContext(ctx, "SELECT value FROM system_config WHERE key=$1", key).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

func (d *DB) SetSystemConfig(ctx context.Context, key, value string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO system_config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()",
		key, value)
	return err
}

// ListSystemConfigOlderThan returns keys under prefix last written before cutoff.
//
// LIKE with an escaped prefix, not a bare concatenation: a prefix containing %
// or _ would otherwise match far more than the caller meant, and the caller is
// a sweeper that deletes what it finds.
func (d *DB) ListSystemConfigOlderThan(ctx context.Context, prefix string, cutoff time.Time) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT key FROM system_config WHERE key LIKE $1 ESCAPE '\' AND updated_at < $2 ORDER BY key`,
		likePrefix(prefix), cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteSystemConfig removes a row. A key that is not there is not an error --
// the sweeper races the claim path, and losing that race is the normal case.
func (d *DB) DeleteSystemConfig(ctx context.Context, key string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM system_config WHERE key=$1", key)
	return err
}

// likePrefix escapes the LIKE metacharacters so a prefix is matched literally.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(prefix) + "%"
}
