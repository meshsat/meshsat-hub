package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- SOS runs (MESHSAT-1584, HeMB-NG stage 2 C10) ---
//
// The apply transaction starts with its insert, so it holds the write lock from its first
// statement, as in msgops.go.

const sosRunCols = `tenant_id, origin, sos_id, bridge_id, run_start_ms, state, rev, bearers, device_id, text,
	lat, lon, has_position, alert_id, started_at, cancelled_at, cancelled_by, cancel_reason, created_at, updated_at`

func parseTimePtr(s string) *time.Time {
	if s == "" {
		return nil
	}
	t := parseTime(s)
	return &t
}

func scanSOSRun(sc interface{ Scan(...any) error }) (store.SOSRun, error) {
	var r store.SOSRun
	var hasPos int
	var started, cancelled, created, updated string
	err := sc.Scan(&r.TenantID, &r.Origin, &r.SOSID, &r.BridgeID, &r.RunStartMs, &r.State, &r.Rev, &r.Bearers,
		&r.DeviceID, &r.Text, &r.Lat, &r.Lon, &hasPos, &r.AlertID, &started, &cancelled, &r.CancelledBy,
		&r.CancelReason, &created, &updated)
	r.HasPosition = hasPos != 0
	r.StartedAt, r.CancelledAt = parseTimePtr(started), parseTimePtr(cancelled)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, err
}

func sosRunArgs(r *store.SOSRun) []any {
	return []any{r.TenantID, r.Origin, r.SOSID, r.BridgeID, r.RunStartMs, r.State, r.Rev, r.Bearers, r.DeviceID, r.Text,
		r.Lat, r.Lon, boolInt(r.HasPosition), r.AlertID, fmtTimePtr(r.StartedAt), fmtTimePtr(r.CancelledAt), r.CancelledBy,
		r.CancelReason, fmtTime(r.CreatedAt), fmtTime(r.UpdatedAt)}
}

func (d *DB) ApplySOSRunEvent(ctx context.Context, ev *store.SOSRunEvent) (store.SOSRunApplied, error) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return store.SOSRunApplied{}, err
	}
	defer func() { _ = tx.Rollback() }()
	fresh := store.NewSOSRun(ev)
	res, err := tx.ExecContext(ctx, `INSERT INTO sos_runs (`+sosRunCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, sosRunArgs(&fresh)...)
	if err != nil {
		return store.SOSRunApplied{}, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := tx.Commit(); err != nil {
			return store.SOSRunApplied{}, err
		}
		return store.SOSRunApplied{Run: fresh, Created: true}, nil
	}
	run, err := scanSOSRun(tx.QueryRowContext(ctx, "SELECT "+sosRunCols+
		" FROM sos_runs WHERE tenant_id=? AND origin=? AND sos_id=?", ev.TenantID, ev.Origin, ev.SOSID))
	if err != nil {
		return store.SOSRunApplied{}, err
	}
	ended := store.ApplySOSEvent(&run, ev)
	if _, err := tx.ExecContext(ctx, `UPDATE sos_runs SET state=?, rev=?, bearers=?, device_id=?, text=?, lat=?, lon=?,
		has_position=?, started_at=?, cancelled_at=?, cancelled_by=?, cancel_reason=?, updated_at=?
		WHERE tenant_id=? AND origin=? AND sos_id=?`,
		run.State, run.Rev, run.Bearers, run.DeviceID, run.Text, run.Lat, run.Lon, boolInt(run.HasPosition),
		fmtTimePtr(run.StartedAt), fmtTimePtr(run.CancelledAt), run.CancelledBy, run.CancelReason, fmtTime(run.UpdatedAt),
		run.TenantID, run.Origin, run.SOSID); err != nil {
		return store.SOSRunApplied{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.SOSRunApplied{}, err
	}
	return store.SOSRunApplied{Run: run, Ended: ended}, nil
}

func (d *DB) GetSOSRun(ctx context.Context, tenantID, origin, sosID string) (*store.SOSRun, error) {
	r, err := scanSOSRun(d.rawDB.QueryRowContext(ctx, "SELECT "+sosRunCols+
		" FROM sos_runs WHERE tenant_id=? AND origin=? AND sos_id=?", tenantID, origin, sosID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) querySOSRuns(ctx context.Context, q string, args ...any) ([]store.SOSRun, error) {
	rows, err := d.rawDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.SOSRun
	for rows.Next() {
		r, err := scanSOSRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) FindSOSRuns(ctx context.Context, tenantID, sosID string) ([]store.SOSRun, error) {
	return d.querySOSRuns(ctx, "SELECT "+sosRunCols+" FROM sos_runs WHERE tenant_id=? AND sos_id=? ORDER BY created_at",
		tenantID, sosID)
}

func (d *DB) ListSOSRuns(ctx context.Context, tenantID string, limit int) ([]store.SOSRun, error) {
	if limit <= 0 {
		limit = 100
	}
	return d.querySOSRuns(ctx, "SELECT "+sosRunCols+" FROM sos_runs WHERE tenant_id=? ORDER BY created_at DESC, sos_id LIMIT ?",
		tenantID, limit)
}

func (d *DB) SetSOSRunAlert(ctx context.Context, tenantID, origin, sosID, alertID string) error {
	res, err := d.rawDB.ExecContext(ctx, `UPDATE sos_runs SET alert_id=?, updated_at=? WHERE tenant_id=? AND origin=? AND sos_id=?`,
		alertID, fmtTime(time.Now().UTC()), tenantID, origin, sosID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
