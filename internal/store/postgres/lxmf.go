package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- LXMF inbox and outbox (MESHSAT-1555) ---

const lxmfCols = "tenant_id, id, hash, direction, state, source, destination, title, content, fields_json, method, stamp_value, signature_valid, detail, created_by_email, message_at, created_at, updated_at"

func scanLXMF(sc interface{ Scan(...any) error }) (*store.LXMFMessage, error) {
	var m store.LXMFMessage
	if err := sc.Scan(&m.TenantID, &m.ID, &m.Hash, &m.Direction, &m.State, &m.Source, &m.Destination, &m.Title, &m.Content,
		&m.FieldsJSON, &m.Method, &m.StampValue, &m.SignatureValid, &m.Detail, &m.CreatedByEmail, &m.MessageAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.MessageAt, m.CreatedAt, m.UpdatedAt = utc(m.MessageAt), utc(m.CreatedAt), utc(m.UpdatedAt)
	return &m, nil
}

func (d *DB) InsertLXMFMessage(ctx context.Context, tenantID string, m *store.LXMFMessage) error {
	if m.ID == "" {
		m.ID = xid.New().String()
	}
	m.TenantID = tenantID
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.MessageAt.IsZero() {
		m.MessageAt = now
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO lxmf_messages (tenant_id, id, hash, direction, state, source, destination, title, content, fields_json, method, stamp_value, signature_valid, detail, created_by_email, message_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) ON CONFLICT DO NOTHING`,
		tenantID, m.ID, m.Hash, m.Direction, m.State, m.Source, m.Destination, m.Title, m.Content, m.FieldsJSON, m.Method, m.StampValue,
		m.SignatureValid, m.Detail, m.CreatedByEmail, m.MessageAt.UTC(), m.CreatedAt.UTC(), m.UpdatedAt.UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) GetLXMFMessage(ctx context.Context, tenantID, id string) (*store.LXMFMessage, error) {
	m, err := scanLXMF(d.db.QueryRowContext(ctx, `SELECT `+lxmfCols+` FROM lxmf_messages WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

func (d *DB) ListLXMFMessages(ctx context.Context, tenantID string, q store.LXMFQuery) ([]store.LXMFMessage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	before := q.Before
	if before.IsZero() {
		before = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+lxmfCols+` FROM lxmf_messages WHERE tenant_id=$1
		AND ($2 = '' OR direction=$2) AND ($3 = '' OR state=$3) AND created_at < $4
		ORDER BY created_at DESC, id DESC LIMIT $5`, tenantID, q.Direction, q.State, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.LXMFMessage{}
	for rows.Next() {
		m, err := scanLXMF(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (d *DB) UpdateLXMFMessageState(ctx context.Context, tenantID, id string, upd store.LXMFStateUpdate) error {
	res, err := d.db.ExecContext(ctx, `UPDATE lxmf_messages SET state=$3,
		hash=CASE WHEN $4 <> '' THEN $4 ELSE hash END,
		method=CASE WHEN $5 <> '' THEN $5 ELSE method END,
		detail=CASE WHEN $6 <> '' THEN $6 ELSE detail END,
		updated_at=$7 WHERE tenant_id=$1 AND id=$2`,
		tenantID, id, upd.State, upd.Hash, upd.Method, upd.Detail, time.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) CountLXMFMessages(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lxmf_messages WHERE tenant_id=$1`, tenantID).Scan(&n)
	return n, err
}

func (d *DB) PruneLXMFMessages(ctx context.Context, tenantID string, keep int) (int, error) {
	if keep < 0 {
		keep = 0
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_messages WHERE tenant_id=$1 AND state NOT IN ($2, $3) AND id IN (
		SELECT id FROM lxmf_messages WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC OFFSET $4)`,
		tenantID, store.LXMFStateQueued, store.LXMFStateSending, keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
