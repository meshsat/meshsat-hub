package postgres

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func (d *DB) LinkOIDCIdentity(ctx context.Context, id *store.OIDCIdentity) error {
	now := time.Now().UTC()
	id.LastLoginAt = now
	if id.CreatedAt.IsZero() {
		id.CreatedAt = now
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO oidc_identities (issuer, subject, user_id, tenant_id, email, platform_admin, last_login_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (issuer, subject) DO UPDATE SET user_id = EXCLUDED.user_id, tenant_id = EXCLUDED.tenant_id, email = EXCLUDED.email,
		platform_admin = EXCLUDED.platform_admin, last_login_at = EXCLUDED.last_login_at`,
		id.Issuer, id.Subject, id.UserID, id.TenantID, id.Email, id.PlatformAdmin, now, id.CreatedAt.UTC())
	return err
}

func (d *DB) IsPlatformAdmin(ctx context.Context, tenantID, userID string) (bool, error) {
	var admin bool
	err := d.db.QueryRowContext(ctx, "SELECT COALESCE(bool_or(platform_admin), FALSE) FROM oidc_identities WHERE tenant_id = $1 AND user_id = $2", tenantID, userID).Scan(&admin)
	return admin, err
}

func (d *DB) GetOIDCIdentity(ctx context.Context, issuer, subject string) (*store.OIDCIdentity, error) {
	var id store.OIDCIdentity
	err := d.db.QueryRowContext(ctx, "SELECT issuer, subject, user_id, tenant_id, email, platform_admin, last_login_at, created_at FROM oidc_identities WHERE issuer = $1 AND subject = $2", issuer, subject).
		Scan(&id.Issuer, &id.Subject, &id.UserID, &id.TenantID, &id.Email, &id.PlatformAdmin, &id.LastLoginAt, &id.CreatedAt)
	if err != nil {
		return nil, err
	}
	id.LastLoginAt, id.CreatedAt = utc(id.LastLoginAt), utc(id.CreatedAt)
	return &id, nil
}

// ClaimOIDCIdentity links an identity only if the subject is unlinked, and
// returns whichever identity holds it afterwards. ON CONFLICT DO NOTHING is
// the whole mechanism: the constraint decides the winner, so two replicas
// serving two callbacks for one new subject cannot both believe they won.
func (d *DB) ClaimOIDCIdentity(ctx context.Context, id *store.OIDCIdentity) (*store.OIDCIdentity, bool, error) {
	now := time.Now().UTC()
	id.LastLoginAt = now
	if id.CreatedAt.IsZero() {
		id.CreatedAt = now
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO oidc_identities (issuer, subject, user_id, tenant_id, email, platform_admin, last_login_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (issuer, subject) DO NOTHING`,
		id.Issuer, id.Subject, id.UserID, id.TenantID, id.Email, id.PlatformAdmin, now, id.CreatedAt.UTC())
	if err != nil {
		return nil, false, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		return id, true, nil
	}
	existing, err := d.GetOIDCIdentity(ctx, id.Issuer, id.Subject)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}
