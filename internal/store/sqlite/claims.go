package sqlite

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Dispatch claims (MESHSAT-910) ---

func (d *DB) ClaimOnce(ctx context.Context, key string) (bool, error) {
	res, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO dispatch_claims (key, claimed_at) VALUES (?, ?)`,
		key, time.Now().UTC().Format(time.DateTime))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) PurgeClaims(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM dispatch_claims WHERE claimed_at < ?`, before.UTC().Format(time.DateTime))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) ClaimScheduledMessage(ctx context.Context, id string) (bool, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE messages SET status = 'sending', claimed_at = ? WHERE id = ? AND status = 'scheduled'`,
		time.Now().UTC().Format(time.DateTime), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) ExpireStaleSends(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.DateTime)
	res, err := d.db.ExecContext(ctx, `UPDATE messages SET status = 'failed', error = 'send claim expired' WHERE status = 'sending' AND claimed_at != '' AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) AdvanceAlert(ctx context.Context, tenantID string, a *store.Alert, expectedNextEscAt time.Time) (bool, error) {
	// An empty tenantID is a wildcard, as in UpdateAlert (the escalation
	// engine processes alerts across tenants).
	query := `UPDATE alerts SET state=?, current_tier=?, retries=?, acked_by=?, acked_at=?, next_esc_at=?, updated_at=?
		WHERE id=? AND next_esc_at=?`
	args := []any{a.State, a.CurrentTier, a.Retries, a.AckedBy, fmtTime(a.AckedAt), fmtTime(a.NextEscAt), fmtTime(a.UpdatedAt),
		a.ID, fmtTime(expectedNextEscAt)}
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

func (d *DB) SaveDeadmanConfig(ctx context.Context, tenantID string, c *store.DeadmanConfig) error {
	c.TenantID = tenantID
	c.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `INSERT INTO deadman_configs (device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_imei, tenant_id) DO UPDATE SET chain_id=excluded.chain_id, interval_sec=excluded.interval_sec, grace_sec=excluded.grace_sec,
		enabled=excluded.enabled, snoozed_until=excluded.snoozed_until, alerted=excluded.alerted, updated_at=excluded.updated_at`,
		c.DeviceIMEI, tenantID, c.ChainID, c.IntervalSec, c.GraceSec, boolToInt(c.Enabled), fmtTime(c.SnoozedUntil), boolToInt(c.Alerted), fmtTime(c.UpdatedAt))
	return err
}

const deadmanCols = "device_imei, tenant_id, chain_id, interval_sec, grace_sec, enabled, snoozed_until, alerted, updated_at"

func scanDeadman(sc interface{ Scan(...any) error }) (store.DeadmanConfig, error) {
	var c store.DeadmanConfig
	var enabled, alerted int
	var snoozed, updated string
	if err := sc.Scan(&c.DeviceIMEI, &c.TenantID, &c.ChainID, &c.IntervalSec, &c.GraceSec, &enabled, &snoozed, &alerted, &updated); err != nil {
		return c, err
	}
	c.Enabled, c.Alerted = enabled == 1, alerted == 1
	c.SnoozedUntil, c.UpdatedAt = parseTime(snoozed), parseTime(updated)
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

// parseTime is the inverse of fmtTime: "" -> zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.DateTime, s)
	return t
}
