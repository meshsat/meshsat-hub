package sqlite

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
		ON CONFLICT(issuer, subject) DO UPDATE SET user_id=excluded.user_id, tenant_id=excluded.tenant_id, email=excluded.email,
		platform_admin=excluded.platform_admin, last_login_at=excluded.last_login_at`,
		id.Issuer, id.Subject, id.UserID, id.TenantID, id.Email, boolToInt(id.PlatformAdmin), fmtTime(now), fmtTime(id.CreatedAt.UTC()))
	return err
}

func (d *DB) IsPlatformAdmin(ctx context.Context, tenantID, userID string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM oidc_identities WHERE tenant_id=? AND user_id=? AND platform_admin=1", tenantID, userID).Scan(&n)
	return n > 0, err
}

func (d *DB) GetOIDCIdentity(ctx context.Context, issuer, subject string) (*store.OIDCIdentity, error) {
	var id store.OIDCIdentity
	var admin int
	var last, created string
	err := d.db.QueryRowContext(ctx, "SELECT issuer, subject, user_id, tenant_id, email, platform_admin, last_login_at, created_at FROM oidc_identities WHERE issuer=? AND subject=?", issuer, subject).
		Scan(&id.Issuer, &id.Subject, &id.UserID, &id.TenantID, &id.Email, &admin, &last, &created)
	if err != nil {
		return nil, err
	}
	id.PlatformAdmin = admin == 1
	id.LastLoginAt, id.CreatedAt = parseTime(last), parseTime(created)
	return &id, nil
}
