package postgres

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Geofences (MESHSAT-1119). Tenant-scoped throughout, and the upsert is keyed
// on (tenant_id, id) rather than id: two tenants may both call a fence
// "perimeter", and an ON CONFLICT on id alone would let the second one take
// over the first's row -- which is the defect webhook_configs actually had.

const geofenceCols = "id, tenant_id, name, polygon, trigger_mode, chain_id, enabled, cooldown_sec, created_at, updated_at"

func scanGeofence(sc interface{ Scan(...any) error }) (store.Geofence, error) {
	var f store.Geofence
	var polygon []byte
	if err := sc.Scan(&f.ID, &f.TenantID, &f.Name, &polygon, &f.Trigger, &f.ChainID,
		&f.Enabled, &f.CooldownSec, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return f, err
	}
	_ = jsonInto(polygon, &f.Polygon)
	f.CreatedAt, f.UpdatedAt = utc(f.CreatedAt), utc(f.UpdatedAt)
	return f, nil
}

func (d *DB) SaveGeofence(ctx context.Context, tenantID string, f *store.Geofence) error {
	polygon, err := jsonBytes(f.Polygon)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO geofences (id, tenant_id, name, polygon, trigger_mode, chain_id, enabled, cooldown_sec, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		 ON CONFLICT (tenant_id, id) DO UPDATE SET name=EXCLUDED.name, polygon=EXCLUDED.polygon,
		   trigger_mode=EXCLUDED.trigger_mode, chain_id=EXCLUDED.chain_id,
		   enabled=EXCLUDED.enabled, cooldown_sec=EXCLUDED.cooldown_sec, updated_at=now()`,
		f.ID, tenantID, f.Name, polygon, f.Trigger, f.ChainID, f.Enabled, f.CooldownSec)
	return err
}

func (d *DB) ListGeofences(ctx context.Context, tenantID string) ([]store.Geofence, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+geofenceCols+" FROM geofences WHERE tenant_id=$1 ORDER BY created_at", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Geofence
	for rows.Next() {
		f, err := scanGeofence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) DeleteGeofence(ctx context.Context, tenantID string, id string) error {
	res, err := d.db.ExecContext(ctx, "DELETE FROM geofences WHERE tenant_id=$1 AND id=$2", tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
