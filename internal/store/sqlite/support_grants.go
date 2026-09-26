package sqlite

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
	var created, expires, used, revoked string
	if err := sc.Scan(&g.TenantID, &g.ID, &g.PINHash, &g.CreatedByUserID, &g.CreatedByEmail, &created, &expires, &used, &g.UsedByEmail, &revoked, &g.FailedAttempts); err != nil {
		return nil, err
	}
	g.CreatedAt, g.ExpiresAt = parseTime(created), parseTime(expires)
	if used != "" {
		x := parseTime(used)
		g.UsedAt = &x
	}
	if revoked != "" {
		x := parseTime(revoked)
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
	if _, err := tx.ExecContext(ctx, `UPDATE support_grants SET revoked_at=? WHERE tenant_id=? AND revoked_at=''`, fmtTime(g.CreatedAt), tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO support_grants (`+supportGrantCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, g.ID, g.PINHash, g.CreatedByUserID, g.CreatedByEmail, fmtTime(g.CreatedAt), fmtTime(g.ExpiresAt.UTC()), fmtTimePtr(g.UsedAt), g.UsedByEmail, fmtTimePtr(g.RevokedAt), g.FailedAttempts); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetActiveSupportGrant(ctx context.Context, tenantID string) (*store.SupportGrant, error) {
	now := fmtTime(time.Now().UTC())
	g, err := scanSupportGrant(d.db.QueryRowContext(ctx,
		`SELECT `+supportGrantCols+` FROM support_grants WHERE tenant_id=? AND revoked_at='' AND expires_at>? ORDER BY created_at DESC LIMIT 1`,
		tenantID, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return g, err
}

func (d *DB) MarkSupportGrantUsed(ctx context.Context, tenantID, id, byEmail string, at time.Time) error {
	res, err := d.db.ExecContext(ctx, `UPDATE support_grants SET used_at=?, used_by_email=? WHERE tenant_id=? AND id=? AND used_at=''`,
		fmtTime(at.UTC()), byEmail, tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already opened, or unknown: either way the row is not changed.
		var exists int
		if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM support_grants WHERE tenant_id=? AND id=?`, tenantID, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return store.ErrNotFound
		}
	}
	return nil
}

func (d *DB) BumpSupportGrantFailures(ctx context.Context, tenantID, id string) (int, error) {
	if _, err := d.db.ExecContext(ctx, `UPDATE support_grants SET failed_attempts=failed_attempts+1 WHERE tenant_id=? AND id=?`, tenantID, id); err != nil {
		return 0, err
	}
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT failed_attempts FROM support_grants WHERE tenant_id=? AND id=?`, tenantID, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	return n, err
}

func (d *DB) RevokeSupportGrant(ctx context.Context, tenantID, id string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE support_grants SET revoked_at=? WHERE tenant_id=? AND id=? AND revoked_at=''`, fmtTime(at.UTC()), tenantID, id)
	return err
}
