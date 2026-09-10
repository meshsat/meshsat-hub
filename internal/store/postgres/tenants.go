package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Tenants (MESHSAT-916) ---

const tenantCols = "id, slug, name, owner_user_id, plan, status, created_at, updated_at, deleted_at, plan_expires_at, kofi_claim_code, kofi_payer_email, kofi_last_message_id, lapse_warned_at"

func scanTenant(sc interface{ Scan(...any) error }) (store.Tenant, error) {
	var t store.Tenant
	var del, expires, warned sql.NullTime
	if err := sc.Scan(&t.ID, &t.Slug, &t.Name, &t.OwnerUserID, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt, &del, &expires, &t.KofiClaimCode, &t.KofiPayerEmail, &t.KofiLastMessageID, &warned); err != nil {
		return t, err
	}
	t.CreatedAt, t.UpdatedAt = utc(t.CreatedAt), utc(t.UpdatedAt)
	if del.Valid {
		d := utc(del.Time)
		t.DeletedAt = &d
	}
	if expires.Valid {
		e := utc(expires.Time)
		t.PlanExpiresAt = &e
	}
	if warned.Valid {
		w := utc(warned.Time)
		t.LapseWarnedAt = &w
	}
	return t, nil
}

func (d *DB) CreateTenant(ctx context.Context, t *store.Tenant) error {
	if t.ID == "" {
		t.ID = xid.New().String()
	}
	if store.ReservedTenantIDs[strings.ToLower(t.ID)] || store.ReservedTenantIDs[strings.ToLower(t.Slug)] {
		return store.ErrReservedTenantID
	}
	if t.Slug == "" {
		t.Slug = t.ID
	}
	if t.Plan == "" {
		// A new tenant starts on the free tier (MESHSAT-989). It used to start
		// on "beta", which now means an unlimited fleet -- the plan rows that
		// predate tiers keep that, deliberately, but a tenant created today
		// must not be grandfathered into something it never signed up for.
		t.Plan = plans.Free
	}
	if t.Status == "" {
		t.Status = "active"
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	_, err := d.db.ExecContext(ctx, `INSERT INTO tenants (id, slug, name, owner_user_id, plan, status, created_at, updated_at, plan_expires_at, kofi_claim_code, kofi_payer_email, kofi_last_message_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t.ID, t.Slug, t.Name, t.OwnerUserID, t.Plan, t.Status, now, now, t.PlanExpiresAt, t.KofiClaimCode, t.KofiPayerEmail, t.KofiLastMessageID)
	return err
}

func (d *DB) GetTenant(ctx context.Context, id string) (*store.Tenant, error) {
	t, err := scanTenant(d.db.QueryRowContext(ctx, "SELECT "+tenantCols+" FROM tenants WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) GetTenantBySlug(ctx context.Context, slug string) (*store.Tenant, error) {
	t, err := scanTenant(d.db.QueryRowContext(ctx, "SELECT "+tenantCols+" FROM tenants WHERE slug = $1", slug))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+tenantCols+" FROM tenants ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) UpdateTenant(ctx context.Context, t *store.Tenant) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `UPDATE tenants SET slug = $1, name = $2, owner_user_id = $3, plan = $4, status = $5, updated_at = $6, plan_expires_at = $7, kofi_claim_code = $8, kofi_payer_email = $9, kofi_last_message_id = $10, lapse_warned_at = $11 WHERE id = $12`,
		t.Slug, t.Name, t.OwnerUserID, t.Plan, t.Status, t.UpdatedAt, t.PlanExpiresAt, t.KofiClaimCode, t.KofiPayerEmail, t.KofiLastMessageID, t.LapseWarnedAt, t.ID)
	return err
}

// ApplyKofiDelivery claims the delivery and grants the plan in one transaction.
// If the grant fails the claim rolls back with it, so Ko-fi's retry still works.
func (d *DB) ApplyKofiDelivery(ctx context.Context, t *store.Tenant, deliveryKey string) (bool, error) {
	if deliveryKey == "" {
		return false, fmt.Errorf("postgres: a Ko-fi delivery key is required")
	}
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO kofi_deliveries (delivery_key, tenant_id, applied_at) VALUES ($1, $2, $3) ON CONFLICT (delivery_key) DO NOTHING`,
		deliveryKey, t.ID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil // already applied
	}

	t.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE tenants SET plan = $1, plan_expires_at = $2, kofi_payer_email = $3, kofi_last_message_id = $4, updated_at = $5 WHERE id = $6`,
		t.Plan, t.PlanExpiresAt, t.KofiPayerEmail, t.KofiLastMessageID, t.UpdatedAt, t.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// --- Tenant invites ---

const inviteCols = "id, tenant_id, email_lower, role, token_hash, expires_at, accepted_at, created_at"

func scanInvite(sc interface{ Scan(...any) error }) (store.TenantInvite, error) {
	var i store.TenantInvite
	var accepted sql.NullTime
	if err := sc.Scan(&i.ID, &i.TenantID, &i.Email, &i.Role, &i.TokenHash, &i.ExpiresAt, &accepted, &i.CreatedAt); err != nil {
		return i, err
	}
	if accepted.Valid {
		i.AcceptedAt = accepted.Time.UTC()
	}
	i.ExpiresAt, i.CreatedAt = utc(i.ExpiresAt), utc(i.CreatedAt)
	return i, nil
}

func (d *DB) CreateInvite(ctx context.Context, tenantID string, inv *store.TenantInvite) error {
	if inv.ID == "" {
		inv.ID = fmt.Sprintf("inv-%s", xid.New().String())
	}
	inv.TenantID = tenantID
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	if inv.Role == "" {
		inv.Role = "viewer"
	}
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = time.Now().UTC().Add(14 * 24 * time.Hour)
	}
	inv.CreatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `INSERT INTO tenant_invites (id, tenant_id, email_lower, role, token_hash, expires_at, accepted_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)`,
		inv.ID, tenantID, inv.Email, inv.Role, inv.TokenHash, inv.ExpiresAt, inv.CreatedAt)
	return err
}

func (d *DB) GetPendingInviteByEmail(ctx context.Context, email string) (*store.TenantInvite, error) {
	i, err := scanInvite(d.db.QueryRowContext(ctx, "SELECT "+inviteCols+" FROM tenant_invites WHERE email_lower = $1 AND accepted_at IS NULL AND expires_at > now() ORDER BY created_at DESC LIMIT 1",
		strings.ToLower(strings.TrimSpace(email))))
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func (d *DB) AcceptInvite(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE tenant_invites SET accepted_at = now() WHERE id = $1 AND accepted_at IS NULL", id)
	return err
}

func (d *DB) ListInvites(ctx context.Context, tenantID string) ([]store.TenantInvite, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+inviteCols+" FROM tenant_invites WHERE tenant_id = $1 ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TenantInvite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (d *DB) DeleteInvite(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM tenant_invites WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

// EnsureClaimCode mints a Ko-fi claim code only if the tenant has none, and
// returns whatever the tenant ends up carrying. The conditional UPDATE is the
// whole point: the loser of a race reads back the winner's code instead of
// walking away with one nobody stored.
//
// A candidate that collides with another tenant's code trips the partial
// unique index and comes back as an error; the caller shows no code and the
// next read mints a different one. At 32^8 candidates that is not a case worth
// retrying in a loop.
func (d *DB) EnsureClaimCode(ctx context.Context, tenantID, candidate string) (string, error) {
	if tenantID == "" || candidate == "" {
		return "", fmt.Errorf("postgres: tenant id and a candidate claim code are required")
	}
	if _, err := d.db.ExecContext(ctx,
		`UPDATE tenants SET kofi_claim_code = $1, updated_at = $2
		 WHERE id = $3 AND kofi_claim_code = ''`,
		candidate, time.Now().UTC(), tenantID); err != nil {
		return "", err
	}
	var code string
	if err := d.db.QueryRowContext(ctx,
		`SELECT kofi_claim_code FROM tenants WHERE id = $1`, tenantID).Scan(&code); err != nil {
		return "", err
	}
	return code, nil
}
