package mariadb

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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE user_id=VALUES(user_id), tenant_id=VALUES(tenant_id), email=VALUES(email),
		platform_admin=VALUES(platform_admin), last_login_at=VALUES(last_login_at)`,
		id.Issuer, id.Subject, id.UserID, id.TenantID, id.Email, id.PlatformAdmin, now, id.CreatedAt.UTC())
	return err
}

func (d *DB) GetOIDCIdentity(ctx context.Context, issuer, subject string) (*store.OIDCIdentity, error) {
	var id store.OIDCIdentity
	err := d.db.QueryRowContext(ctx, "SELECT issuer, subject, user_id, tenant_id, email, platform_admin, last_login_at, created_at FROM oidc_identities WHERE issuer=? AND subject=?", issuer, subject).
		Scan(&id.Issuer, &id.Subject, &id.UserID, &id.TenantID, &id.Email, &id.PlatformAdmin, &id.LastLoginAt, &id.CreatedAt)
	if err != nil {
		return nil, err
	}
	id.LastLoginAt, id.CreatedAt = id.LastLoginAt.UTC(), id.CreatedAt.UTC()
	return &id, nil
}
