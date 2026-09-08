package mariadb

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Dispatch claims (MESHSAT-910) ---

func (d *DB) ClaimOnce(ctx context.Context, key string) (bool, error) {
	_, err := d.db.ExecContext(ctx, "INSERT INTO dispatch_claims (`key`, claimed_at) VALUES (?, ?)", key, time.Now().UTC())
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) PurgeClaims(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, "DELETE FROM dispatch_claims WHERE claimed_at < ?", before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) ClaimScheduledMessage(ctx context.Context, id string) (bool, error) {
	res, err := d.db.ExecContext(ctx, "UPDATE messages SET status = 'sending', claimed_at = ? WHERE id = ? AND status = 'scheduled'", time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) ExpireStaleSends(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := d.db.ExecContext(ctx, "UPDATE messages SET status = 'failed', error = 'send claim expired' WHERE status = 'sending' AND claimed_at IS NOT NULL AND claimed_at < ?", time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) AdvanceAlert(ctx context.Context, tenantID string, a *store.Alert, expectedNextEscAt time.Time) (bool, error) {
	// An empty tenantID is a wildcard, as in UpdateAlert.
	query := `UPDATE alerts SET state=?, current_tier=?, retries=?, acked_by=?, acked_at=?, next_esc_at=?, updated_at=?
		WHERE id=? AND next_esc_at=?`
	args := []any{a.State, a.CurrentTier, a.Retries, a.AckedBy, a.AckedAt.UTC(), a.NextEscAt.UTC(), a.UpdatedAt.UTC(),
		a.ID, expectedNextEscAt.UTC()}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	res, err := d.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// --- Dead man's switch configs ---

func nullTimeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (d *DB) SaveDeadmanConfig(ctx context.Context, tenantID string, c *store.DeadmanConfig) error {
	c.TenantID = tenantID
	c.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `INSERT INTO deadman_configs (device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE chain_id=VALUES(chain_id), interval_sec=VALUES(interval_sec), grace_sec=VALUES(grace_sec),
		enabled=VALUES(enabled), snoozed_until=VALUES(snoozed_until), alerted=VALUES(alerted), updated_at=VALUES(updated_at)`,
		c.DeviceIMEI, tenantID, c.ChainID, c.IntervalSec, c.GraceSec, c.Enabled, nullTimeArg(c.SnoozedUntil), c.Alerted, c.UpdatedAt)
	return err
}

const deadmanCols = "device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at"

func scanDeadman(sc interface{ Scan(...any) error }) (store.DeadmanConfig, error) {
	var c store.DeadmanConfig
	var snoozed sql.NullTime
	if err := sc.Scan(&c.DeviceIMEI, &c.TenantID, &c.ChainID, &c.IntervalSec, &c.GraceSec, &c.Enabled, &snoozed, &c.Alerted, &c.UpdatedAt); err != nil {
		return c, err
	}
	if snoozed.Valid {
		c.SnoozedUntil = snoozed.Time.UTC()
	}
	c.UpdatedAt = c.UpdatedAt.UTC()
	return c, nil
}

func (d *DB) GetDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeadmanConfig, error) {
	c, err := scanDeadman(d.db.QueryRowContext(ctx, "SELECT "+deadmanCols+" FROM deadman_configs WHERE device_imei=? AND tenant_id=?", deviceIMEI, tenantID))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListDeadmanConfigs(ctx context.Context) ([]store.DeadmanConfig, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+deadmanCols+" FROM deadman_configs ORDER BY tenant_id, device_imei")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.DeadmanConfig
	for rows.Next() {
		c, err := scanDeadman(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) DeleteDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM deadman_configs WHERE device_imei=? AND tenant_id=?", deviceIMEI, tenantID)
	return err
}
