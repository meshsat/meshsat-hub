package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- SOS runs (MESHSAT-1584, HeMB-NG stage 2 C10) ---

const sosRunCols = `tenant_id, origin, sos_id, bridge_id, run_start_ms, state, rev, bearers, device_id, text,
	lat, lon, has_position, alert_id, started_at, cancelled_at, cancelled_by, cancel_reason, created_at, updated_at`

func scanSOSRun(sc interface{ Scan(...any) error }) (store.SOSRun, error) {
	var r store.SOSRun
	var started, cancelled sql.NullTime
	err := sc.Scan(&r.TenantID, &r.Origin, &r.SOSID, &r.BridgeID, &r.RunStartMs, &r.State, &r.Rev, &r.Bearers,
		&r.DeviceID, &r.Text, &r.Lat, &r.Lon, &r.HasPosition, &r.AlertID, &started, &cancelled, &r.CancelledBy,
		&r.CancelReason, &r.CreatedAt, &r.UpdatedAt)
	r.StartedAt, r.CancelledAt = utcPtr(started), utcPtr(cancelled)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	return r, err
}

func sosRunArgs(r *store.SOSRun) []any {
	return []any{r.TenantID, r.Origin, r.SOSID, r.BridgeID, r.RunStartMs, r.State, r.Rev, r.Bearers, r.DeviceID, r.Text,
		r.Lat, r.Lon, r.HasPosition, r.AlertID, nullTimePtr(r.StartedAt), nullTimePtr(r.CancelledAt), r.CancelledBy,
		r.CancelReason, r.CreatedAt.UTC(), r.UpdatedAt.UTC()}
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

	// A new run is inserted outright; a concurrent insert of the same key waits for the
	// other transaction and then finds the row, so two replicas never both create it.
	fresh := store.NewSOSRun(ev)
	res, err := tx.ExecContext(ctx, `INSERT INTO sos_runs (`+sosRunCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) ON CONFLICT DO NOTHING`,
		sosRunArgs(&fresh)...)
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
		" FROM sos_runs WHERE tenant_id=$1 AND origin=$2 AND sos_id=$3 FOR UPDATE", ev.TenantID, ev.Origin, ev.SOSID))
	if err != nil {
		return store.SOSRunApplied{}, err
	}
	ended := store.ApplySOSEvent(&run, ev)
	if _, err := tx.ExecContext(ctx, `UPDATE sos_runs SET state=$4, rev=$5, bearers=$6, device_id=$7, text=$8, lat=$9, lon=$10,
		has_position=$11, started_at=$12, cancelled_at=$13, cancelled_by=$14, cancel_reason=$15, updated_at=$16
		WHERE tenant_id=$1 AND origin=$2 AND sos_id=$3`,
		run.TenantID, run.Origin, run.SOSID, run.State, run.Rev, run.Bearers, run.DeviceID, run.Text, run.Lat, run.Lon,
		run.HasPosition, nullTimePtr(run.StartedAt), nullTimePtr(run.CancelledAt), run.CancelledBy, run.CancelReason,
		run.UpdatedAt.UTC()); err != nil {
		return store.SOSRunApplied{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.SOSRunApplied{}, err
	}
	return store.SOSRunApplied{Run: run, Ended: ended}, nil
}

func (d *DB) GetSOSRun(ctx context.Context, tenantID, origin, sosID string) (*store.SOSRun, error) {
	r, err := scanSOSRun(d.rawDB.QueryRowContext(ctx, "SELECT "+sosRunCols+
		" FROM sos_runs WHERE tenant_id=$1 AND origin=$2 AND sos_id=$3", tenantID, origin, sosID))
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
	return d.querySOSRuns(ctx, "SELECT "+sosRunCols+" FROM sos_runs WHERE tenant_id=$1 AND sos_id=$2 ORDER BY created_at",
		tenantID, sosID)
}

func (d *DB) ListSOSRuns(ctx context.Context, tenantID string, limit int) ([]store.SOSRun, error) {
	if limit <= 0 {
		limit = 100
	}
	return d.querySOSRuns(ctx, "SELECT "+sosRunCols+" FROM sos_runs WHERE tenant_id=$1 ORDER BY created_at DESC, sos_id LIMIT $2",
		tenantID, limit)
}

func (d *DB) SetSOSRunAlert(ctx context.Context, tenantID, origin, sosID, alertID string) error {
	res, err := d.rawDB.ExecContext(ctx, `UPDATE sos_runs SET alert_id=$4, updated_at=$5 WHERE tenant_id=$1 AND origin=$2 AND sos_id=$3`,
		tenantID, origin, sosID, alertID, time.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
