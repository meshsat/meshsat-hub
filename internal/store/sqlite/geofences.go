package sqlite

import (
	"context"
	"encoding/json"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Geofences (MESHSAT-1119). See the postgres implementation for why the key is
// (tenant_id, id) and not id.

const geofenceCols = "id, tenant_id, name, polygon, trigger_mode, chain_id, enabled, cooldown_sec, created_at, updated_at"

func scanGeofence(sc interface{ Scan(...any) error }) (store.Geofence, error) {
	var f store.Geofence
	var polygon string
	var enabled int
	var created, updated string
	if err := sc.Scan(&f.ID, &f.TenantID, &f.Name, &polygon, &f.Trigger, &f.ChainID,
		&enabled, &f.CooldownSec, &created, &updated); err != nil {
		return f, err
	}
	_ = json.Unmarshal([]byte(polygon), &f.Polygon)
	f.Enabled = enabled != 0
	f.CreatedAt, f.UpdatedAt = parseTime(created), parseTime(updated)
	return f, nil
}

func (d *DB) SaveGeofence(ctx context.Context, tenantID string, f *store.Geofence) error {
	polygon, err := json.Marshal(f.Polygon)
	if err != nil {
		return err
	}
	enabled := 0
	if f.Enabled {
		enabled = 1
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO geofences (id, tenant_id, name, polygon, trigger_mode, chain_id, enabled, cooldown_sec, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT (tenant_id, id) DO UPDATE SET name=excluded.name, polygon=excluded.polygon,
		   trigger_mode=excluded.trigger_mode, chain_id=excluded.chain_id,
		   enabled=excluded.enabled, cooldown_sec=excluded.cooldown_sec, updated_at=datetime('now')`,
		f.ID, tenantID, f.Name, string(polygon), f.Trigger, f.ChainID, enabled, f.CooldownSec)
	return err
}

func (d *DB) ListGeofences(ctx context.Context, tenantID string) ([]store.Geofence, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+geofenceCols+" FROM geofences WHERE tenant_id=? ORDER BY created_at", tenantID)
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
	res, err := d.db.ExecContext(ctx, "DELETE FROM geofences WHERE tenant_id=? AND id=?", tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
