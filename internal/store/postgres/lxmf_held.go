package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- LXMF propagation node store (MESHSAT-1555 P5) ---

func (d *DB) InsertLXMFHeld(ctx context.Context, tenantID string, m *store.LXMFHeldMessage) error {
	m.TenantID = tenantID
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now().UTC()
	}
	if m.Size == 0 {
		m.Size = len(m.Data)
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO lxmf_held (tenant_id, id, destination, data, size, stamp_value, source, received_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		tenantID, m.ID, m.Destination, m.Data, m.Size, m.StampValue, m.Source, m.ReceivedAt.UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListLXMFHeldFor(ctx context.Context, tenantID, destination string) ([]store.LXMFHeldMessage, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT tenant_id, id, destination, size, stamp_value, source, received_at FROM lxmf_held WHERE tenant_id=$1 AND destination=$2 ORDER BY size, id`, tenantID, destination)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.LXMFHeldMessage{}
	for rows.Next() {
		var m store.LXMFHeldMessage
		if err := rows.Scan(&m.TenantID, &m.ID, &m.Destination, &m.Size, &m.StampValue, &m.Source, &m.ReceivedAt); err != nil {
			return nil, err
		}
		m.ReceivedAt = utc(m.ReceivedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) GetLXMFHeld(ctx context.Context, tenantID, id string) (*store.LXMFHeldMessage, error) {
	var m store.LXMFHeldMessage
	err := d.db.QueryRowContext(ctx, `SELECT tenant_id, id, destination, data, size, stamp_value, source, received_at FROM lxmf_held WHERE tenant_id=$1 AND id=$2`, tenantID, id).
		Scan(&m.TenantID, &m.ID, &m.Destination, &m.Data, &m.Size, &m.StampValue, &m.Source, &m.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.ReceivedAt = utc(m.ReceivedAt)
	return &m, nil
}

func (d *DB) DeleteLXMFHeld(ctx context.Context, tenantID, destination string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_held WHERE tenant_id=$1 AND destination=$2 AND id = ANY($3)`, tenantID, destination, ids)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (d *DB) LXMFHeldBytes(ctx context.Context, tenantID string) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM lxmf_held WHERE tenant_id=$1`, tenantID).Scan(&n)
	return n, err
}

func (d *DB) PruneLXMFHeld(ctx context.Context, tenantID string, cutoff time.Time, maxBytes int64) (int, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_held WHERE tenant_id=$1 AND received_at < $2`, tenantID, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if maxBytes <= 0 {
		return int(n), nil
	}
	// Oldest first until the total fits: one statement over a running sum.
	res, err = d.db.ExecContext(ctx, `DELETE FROM lxmf_held WHERE tenant_id=$1 AND id IN (
		SELECT id FROM (SELECT id, SUM(size) OVER (ORDER BY received_at DESC, id DESC) AS running FROM lxmf_held WHERE tenant_id=$1) t WHERE t.running > $2)`,
		tenantID, maxBytes)
	if err != nil {
		return int(n), err
	}
	n2, _ := res.RowsAffected()
	return int(n + n2), nil
}
