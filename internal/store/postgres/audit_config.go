package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Audit log, device config versioning and system config. Semantics mirror
// internal/store/mariadb (same generated IDs, same ordering and limits, same
// tenant scoping, not-found = sql.ErrNoRows).

// --- Audit log ---

const auditColumns = "id, action, actor, detail, ip, prev_hash, hash, created_at"

func scanAuditEntry(sc interface{ Scan(...any) error }, a *store.AuditEntry) error {
	if err := sc.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt); err != nil {
		return err
	}
	a.CreatedAt = utc(a.CreatedAt)
	return nil
}

func (d *DB) InsertAuditEntry(ctx context.Context, tenantID string, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip, prev_hash, hash, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP, a.PrevHash, a.Hash, tenantID)
	return err
}

func (d *DB) ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]store.AuditEntry, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+auditColumns+" FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2", tenantID, limit)
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
	row := d.db.QueryRowContext(ctx,
		"SELECT "+auditColumns+" FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1", tenantID)
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
