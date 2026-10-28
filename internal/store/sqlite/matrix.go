package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The Matrix bridge (MESHSAT-1736); see the Postgres twin. Timestamps as
// time.DateTime strings, like the booth's, so max() and < compare them in
// time order.

func (d *DB) UpsertMatrixOperator(ctx context.Context, o *store.MatrixOperator) error {
	if o == nil {
		return errors.New("sqlite: nil matrix operator")
	}
	used := o.LastUsedAt
	if used.IsZero() {
		used = time.Now()
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO matrix_operators (tenant_id, user_id, room_id, matrix_user_id, display_name, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, user_id) DO UPDATE
		   SET room_id = excluded.room_id, matrix_user_id = excluded.matrix_user_id,
		       display_name = excluded.display_name,
		       last_used_at = max(matrix_operators.last_used_at, excluded.last_used_at)`,
		o.TenantID, o.UserID, o.RoomID, o.MatrixUserID, o.DisplayName, boothTime(used)); err != nil {
		return err
	}
	_, _ = d.db.ExecContext(ctx,
		`DELETE FROM matrix_operators WHERE tenant_id = ? AND last_used_at < ?`,
		o.TenantID, boothTime(time.Now().Add(-30*24*time.Hour)))
	return nil
}

func (d *DB) GetMatrixOperator(ctx context.Context, tenantID, userID string) (*store.MatrixOperator, error) {
	var o store.MatrixOperator
	var used string
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, user_id, room_id, matrix_user_id, display_name, last_used_at
		 FROM matrix_operators WHERE tenant_id = ? AND user_id = ?`, tenantID, userID).
		Scan(&o.TenantID, &o.UserID, &o.RoomID, &o.MatrixUserID, &o.DisplayName, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.LastUsedAt = boothParse(used)
	return &o, nil
}

func (d *DB) GetMatrixSync(ctx context.Context, tenantID string) (*store.MatrixSync, error) {
	var s store.MatrixSync
	var updated string
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, bot_user_id, next_batch, updated_at FROM matrix_sync WHERE tenant_id = ?`, tenantID).
		Scan(&s.TenantID, &s.BotUserID, &s.NextBatch, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.UpdatedAt = boothParse(updated)
	return &s, nil
}

func (d *DB) SaveMatrixSync(ctx context.Context, s *store.MatrixSync) error {
	if s == nil {
		return errors.New("sqlite: nil matrix sync")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO matrix_sync (tenant_id, bot_user_id, next_batch, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (tenant_id) DO UPDATE
		   SET bot_user_id = excluded.bot_user_id, next_batch = excluded.next_batch, updated_at = excluded.updated_at`,
		s.TenantID, s.BotUserID, s.NextBatch, boothTime(time.Now()))
	return err
}
