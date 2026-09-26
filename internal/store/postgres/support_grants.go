package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Support access grants (MESHSAT-1366) ---

const supportGrantCols = "tenant_id, id, pin_hash, created_by_user_id, created_by_email, created_at, expires_at, used_at, used_by_email, revoked_at, failed_attempts"

func scanSupportGrant(sc interface{ Scan(...any) error }) (*store.SupportGrant, error) {
	var g store.SupportGrant
	var used, revoked sql.NullTime
	if err := sc.Scan(&g.TenantID, &g.ID, &g.PINHash, &g.CreatedByUserID, &g.CreatedByEmail, &g.CreatedAt, &g.ExpiresAt, &used, &g.UsedByEmail, &revoked, &g.FailedAttempts); err != nil {
		return nil, err
	}
	g.CreatedAt, g.ExpiresAt = utc(g.CreatedAt), utc(g.ExpiresAt)
	if used.Valid {
		x := utc(used.Time)
		g.UsedAt = &x
	}
	if revoked.Valid {
		x := utc(revoked.Time)
		g.RevokedAt = &x
	}
	return &g, nil
}

// CreateSupportGrant writes a grant and revokes every earlier unrevoked one
// of the tenant: a customer has one open window at a time.
func (d *DB) CreateSupportGrant(ctx context.Context, tenantID string, g *store.SupportGrant) error {
	if g.ID == "" {
		g.ID = xid.New().String()
	}
	g.TenantID = tenantID
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE support_grants SET revoked_at=$1 WHERE tenant_id=$2 AND revoked_at IS NULL`, g.CreatedAt, tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO support_grants (`+supportGrantCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		tenantID, g.ID, g.PINHash, g.CreatedByUserID, g.CreatedByEmail, g.CreatedAt, g.ExpiresAt.UTC(), g.UsedAt, g.UsedByEmail, g.RevokedAt, g.FailedAttempts); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetActiveSupportGrant(ctx context.Context, tenantID string) (*store.SupportGrant, error) {
	g, err := scanSupportGrant(d.db.QueryRowContext(ctx,
		`SELECT `+supportGrantCols+` FROM support_grants WHERE tenant_id=$1 AND revoked_at IS NULL AND expires_at>$2 ORDER BY created_at DESC LIMIT 1`,
		tenantID, time.Now().UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return g, err
}

func (d *DB) MarkSupportGrantUsed(ctx context.Context, tenantID, id, byEmail string, at time.Time) error {
	res, err := d.db.ExecContext(ctx, `UPDATE support_grants SET used_at=$1, used_by_email=$2 WHERE tenant_id=$3 AND id=$4 AND used_at IS NULL`,
		at.UTC(), byEmail, tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM support_grants WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return store.ErrNotFound
		}
	}
	return nil
}

func (d *DB) BumpSupportGrantFailures(ctx context.Context, tenantID, id string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `UPDATE support_grants SET failed_attempts=failed_attempts+1 WHERE tenant_id=$1 AND id=$2 RETURNING failed_attempts`, tenantID, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	return n, err
}

func (d *DB) RevokeSupportGrant(ctx context.Context, tenantID, id string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE support_grants SET revoked_at=$1 WHERE tenant_id=$2 AND id=$3 AND revoked_at IS NULL`, at.UTC(), tenantID, id)
	return err
}
