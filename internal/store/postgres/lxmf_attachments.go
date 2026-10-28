package postgres

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
	if err := sc.Scan(&a.TenantID, &a.MessageID, &a.Index, &a.Kind, &a.Mode, &a.Format, &a.Size, &a.ObjectKey, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.CreatedAt = utc(a.CreatedAt)
	return &a, nil
}

func (d *DB) InsertLXMFAttachment(ctx context.Context, tenantID string, a *store.LXMFAttachment) error {
	a.TenantID = tenantID
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO lxmf_attachments (tenant_id, message_id, idx, kind, mode, format, size, object_key, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
		tenantID, a.MessageID, a.Index, a.Kind, a.Mode, a.Format, a.Size, a.ObjectKey, a.CreatedAt.UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListLXMFAttachments(ctx context.Context, tenantID, messageID string) ([]store.LXMFAttachment, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=$1 AND message_id=$2 ORDER BY idx`, tenantID, messageID)
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
	a, err := scanLXMFAttachment(d.db.QueryRowContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=$1 AND message_id=$2 AND idx=$3`, tenantID, messageID, index))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

func (d *DB) DeleteLXMFAttachment(ctx context.Context, tenantID, messageID string, index int) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM lxmf_attachments WHERE tenant_id=$1 AND message_id=$2 AND idx=$3`, tenantID, messageID, index)
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
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM lxmf_attachments WHERE tenant_id=$1`, tenantID).Scan(&n)
	return n, err
}

func (d *DB) OldestLXMFAttachments(ctx context.Context, tenantID string, limit int) ([]store.LXMFAttachment, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+lxmfAttachmentCols+` FROM lxmf_attachments WHERE tenant_id=$1 ORDER BY created_at, message_id, idx LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectLXMFAttachments(rows)
}
