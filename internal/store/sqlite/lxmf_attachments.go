package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- LXMF attachments (MESHSAT-1735) ---

const lxmfAttachmentCols = "tenant_id, message_id, idx, kind, mode, format, size, object_key, created_at"

func scanLXMFAttachment(sc interface{ Scan(...any) error }) (*store.LXMFAttachment, error) {
	var a store.LXMFAttachment
	var created string
	if err := sc.Scan(&a.TenantID, &a.MessageID, &a.Index, &a.Kind, &a.Mode, &a.Format, &a.Size, &a.ObjectKey, &created); err != nil {
		return nil, err
	}
	a.CreatedAt = parseTime(created)
	return &a, nil
}

func (d *DB) InsertLXMFAttachment(ctx context.Context, tenantID string, a *store.LXMFAttachment) error {
	a.TenantID = tenantID
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO lxmf_attachments (tenant_id, message_id, idx, kind, mode, format, size, object_key, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		tenantID, a.MessageID, a.Index, a.Kind, a.Mode, a.Format, a.Size, a.ObjectKey, fmtTime(a.CreatedAt.UTC()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListLXMFAttachments(ctx context.Context, tenantID, messageID string) ([]store.LXMFAttachment, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=? AND message_id=? ORDER BY idx`, tenantID, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectLXMFAttachments(rows)
}

func collectLXMFAttachments(rows *sql.Rows) ([]store.LXMFAttachment, error) {
	out := []store.LXMFAttachment{}
	for rows.Next() {
		a, err := scanLXMFAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (d *DB) GetLXMFAttachment(ctx context.Context, tenantID, messageID string, index int) (*store.LXMFAttachment, error) {
	a, err := scanLXMFAttachment(d.db.QueryRowContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=? AND message_id=? AND idx=?`, tenantID, messageID, index))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

func (d *DB) DeleteLXMFAttachment(ctx context.Context, tenantID, messageID string, index int) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_attachments WHERE tenant_id=? AND message_id=? AND idx=?`, tenantID, messageID, index)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) LXMFAttachmentBytes(ctx context.Context, tenantID string) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM lxmf_attachments WHERE tenant_id=?`, tenantID).Scan(&n)
	return n, err
}

func (d *DB) OldestLXMFAttachments(ctx context.Context, tenantID string, limit int) ([]store.LXMFAttachment, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=? ORDER BY created_at, message_id, idx LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectLXMFAttachments(rows)
}
