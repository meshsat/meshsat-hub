package sqlite

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

// lxmfSelect reads a message with its attachment count (MESHSAT-1735); the
// count is correlated on the tenant as well as the id, since another tenant
// may hold the same message id.
const lxmfSelect = `SELECT ` + lxmfCols + `, (SELECT COUNT(*) FROM lxmf_attachments a WHERE a.tenant_id = m.tenant_id AND a.message_id = m.id) FROM lxmf_messages m`

func scanLXMF(sc interface{ Scan(...any) error }) (*store.LXMFMessage, error) {
	var m store.LXMFMessage
	var sig int
	var msgAt, created, updated string
	if err := sc.Scan(&m.TenantID, &m.ID, &m.Hash, &m.Direction, &m.State, &m.Source, &m.Destination, &m.Title, &m.Content,
		&m.FieldsJSON, &m.Method, &m.StampValue, &sig, &m.Detail, &m.CreatedByEmail, &msgAt, &created, &updated, &m.Attachments); err != nil {
		return nil, err
	}
	m.SignatureValid = sig != 0
	m.MessageAt, m.CreatedAt, m.UpdatedAt = parseTime(msgAt), parseTime(created), parseTime(updated)
	return &m, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	res, err := d.db.ExecContext(ctx, `INSERT INTO lxmf_messages (tenant_id, id, hash, direction, state, source, destination, title, content, fields_json, method, stamp_value, signature_valid, detail, created_by_email, message_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		tenantID, m.ID, m.Hash, m.Direction, m.State, m.Source, m.Destination, m.Title, m.Content, m.FieldsJSON, m.Method, m.StampValue,
		boolInt(m.SignatureValid), m.Detail, m.CreatedByEmail, fmtTime(m.MessageAt.UTC()), fmtTime(m.CreatedAt.UTC()), fmtTime(m.UpdatedAt.UTC()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) GetLXMFMessage(ctx context.Context, tenantID, id string) (*store.LXMFMessage, error) {
	m, err := scanLXMF(d.db.QueryRowContext(ctx, lxmfSelect+` WHERE tenant_id=? AND id=?`, tenantID, id))
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
	rows, err := d.db.QueryContext(ctx, lxmfSelect+` WHERE tenant_id=?
		AND (? = '' OR direction=?) AND (? = '' OR state=?) AND created_at < ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, tenantID, q.Direction, q.Direction, q.State, q.State, fmtTime(before.UTC()), limit)
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
	res, err := d.db.ExecContext(ctx, `UPDATE lxmf_messages SET state=?,
		hash=CASE WHEN ? <> '' THEN ? ELSE hash END,
		method=CASE WHEN ? <> '' THEN ? ELSE method END,
		detail=CASE WHEN ? <> '' THEN ? ELSE detail END,
		updated_at=? WHERE tenant_id=? AND id=?`,
		upd.State, upd.Hash, upd.Hash, upd.Method, upd.Method, upd.Detail, upd.Detail, fmtTime(time.Now().UTC()), tenantID, id)
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
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lxmf_messages WHERE tenant_id=?`, tenantID).Scan(&n)
	return n, err
}

func (d *DB) PruneLXMFMessages(ctx context.Context, tenantID string, keep int) (int, error) {
	if keep < 0 {
		keep = 0
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_messages WHERE tenant_id=? AND state NOT IN (?, ?) AND id IN (
		SELECT id FROM lxmf_messages WHERE tenant_id=? ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?)`,
		tenantID, store.LXMFStateQueued, store.LXMFStateSending, tenantID, keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
