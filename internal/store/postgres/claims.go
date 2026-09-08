package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Dispatch claims (MESHSAT-910) ---

func (d *DB) ClaimOnce(ctx context.Context, key string) (bool, error) {
	res, err := d.db.ExecContext(ctx, "INSERT INTO dispatch_claims (key, claimed_at) VALUES ($1, now()) ON CONFLICT (key) DO NOTHING", key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) PurgeClaims(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, "DELETE FROM dispatch_claims WHERE claimed_at < $1", before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) ClaimScheduledMessage(ctx context.Context, id string) (bool, error) {
	res, err := d.db.ExecContext(ctx, "UPDATE messages SET status = 'sending', claimed_at = now() WHERE id = $1 AND status = 'scheduled'", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) ExpireStaleSends(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := d.db.ExecContext(ctx, "UPDATE messages SET status = 'failed', error = 'send claim expired' WHERE status = 'sending' AND claimed_at IS NOT NULL AND claimed_at < $1", time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) AdvanceAlert(ctx context.Context, tenantID string, a *store.Alert, expectedNextEscAt time.Time) (bool, error) {
	// An empty tenantID is a wildcard, as in UpdateAlert.
	query := `UPDATE alerts SET state = $1, current_tier = $2, retries = $3, acked_by = $4, acked_at = $5, next_esc_at = $6, updated_at = $7
		WHERE id = $8 AND next_esc_at = $9`
	args := []any{a.State, a.CurrentTier, a.Retries, a.AckedBy, sentinelTime(a.AckedAt), sentinelTime(a.NextEscAt), a.UpdatedAt.UTC(),
		a.ID, sentinelTime(expectedNextEscAt)}
	if tenantID != "" {
		query += " AND tenant_id = $10"
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

func (d *DB) SaveDeadmanConfig(ctx context.Context, tenantID string, c *store.DeadmanConfig) error {
	c.TenantID = tenantID
	c.UpdatedAt = time.Now().UTC()
	var snoozed any
	if !c.SnoozedUntil.IsZero() {
		snoozed = c.SnoozedUntil.UTC()
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO deadman_configs (device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (device_imei, tenant_id) DO UPDATE SET chain_id = EXCLUDED.chain_id, interval_sec = EXCLUDED.interval_sec, grace_sec = EXCLUDED.grace_sec,
		enabled = EXCLUDED.enabled, snoozed_until = EXCLUDED.snoozed_until, alerted = EXCLUDED.alerted, updated_at = EXCLUDED.updated_at`,
		c.DeviceIMEI, tenantID, c.ChainID, c.IntervalSec, c.GraceSec, c.Enabled, snoozed, c.Alerted, c.UpdatedAt)
	return err
}

const deadmanCols = "device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at"

func scanDeadman(sc interface{ Scan(...any) error }) (store.DeadmanConfig, error) {
	var c store.DeadmanConfig
	var snoozed sql.NullTime
	var updated time.Time
	if err := sc.Scan(&c.DeviceIMEI, &c.TenantID, &c.ChainID, &c.IntervalSec, &c.GraceSec, &c.Enabled, &snoozed, &c.Alerted, &updated); err != nil {
		return c, err
	}
	if snoozed.Valid {
		c.SnoozedUntil = snoozed.Time.UTC()
	}
	c.UpdatedAt = utc(updated)
	return c, nil
}

func (d *DB) GetDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeadmanConfig, error) {
	c, err := scanDeadman(d.db.QueryRowContext(ctx, "SELECT "+deadmanCols+" FROM deadman_configs WHERE device_imei = $1 AND tenant_id = $2", deviceIMEI, tenantID))
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
	_, err := d.db.ExecContext(ctx, "DELETE FROM deadman_configs WHERE device_imei = $1 AND tenant_id = $2", deviceIMEI, tenantID)
	return err
}
