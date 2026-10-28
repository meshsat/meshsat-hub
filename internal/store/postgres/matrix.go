package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The Matrix bridge (MESHSAT-1736), tenant-scoped throughout. See
// store.MatrixOperator and store.MatrixSync.

// matrixOperatorKeep is how long an operator row outlives its last message.
const matrixOperatorKeep = "30 days"

// UpsertMatrixOperator records that a Matrix user wrote to a crew from a room,
// and drops the tenant's rows unused for 30 days.
func (d *DB) UpsertMatrixOperator(ctx context.Context, o *store.MatrixOperator) error {
	if o == nil {
		return errors.New("postgres: nil matrix operator")
	}
	used := o.LastUsedAt
	if used.IsZero() {
		used = time.Now()
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO matrix_operators (tenant_id, user_id, room_id, matrix_user_id, display_name, last_used_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, user_id) DO UPDATE
		   SET room_id = EXCLUDED.room_id, matrix_user_id = EXCLUDED.matrix_user_id,
		       display_name = EXCLUDED.display_name,
		       last_used_at = GREATEST(matrix_operators.last_used_at, EXCLUDED.last_used_at)`,
		o.TenantID, o.UserID, o.RoomID, o.MatrixUserID, o.DisplayName, used.UTC()); err != nil {
		return err
	}
	_, _ = d.db.ExecContext(ctx,
		`DELETE FROM matrix_operators WHERE tenant_id = $1 AND last_used_at < now() - interval '`+matrixOperatorKeep+`'`,
		o.TenantID)
	return nil
}

func (d *DB) GetMatrixOperator(ctx context.Context, tenantID, userID string) (*store.MatrixOperator, error) {
	var o store.MatrixOperator
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, user_id, room_id, matrix_user_id, display_name, last_used_at
		 FROM matrix_operators WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).
		Scan(&o.TenantID, &o.UserID, &o.RoomID, &o.MatrixUserID, &o.DisplayName, &o.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (d *DB) GetMatrixSync(ctx context.Context, tenantID string) (*store.MatrixSync, error) {
	var s store.MatrixSync
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, bot_user_id, next_batch, updated_at FROM matrix_sync WHERE tenant_id = $1`, tenantID).
		Scan(&s.TenantID, &s.BotUserID, &s.NextBatch, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) SaveMatrixSync(ctx context.Context, s *store.MatrixSync) error {
	if s == nil {
		return errors.New("postgres: nil matrix sync")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO matrix_sync (tenant_id, bot_user_id, next_batch, updated_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (tenant_id) DO UPDATE
		   SET bot_user_id = EXCLUDED.bot_user_id, next_batch = EXCLUDED.next_batch, updated_at = now()`,
		s.TenantID, s.BotUserID, s.NextBatch)
	return err
}
