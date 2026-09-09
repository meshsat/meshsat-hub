package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Tenants (MESHSAT-916) ---

const tenantCols = "id, slug, name, owner_user_id, plan, status, created_at, updated_at, deleted_at, plan_expires_at, kofi_claim_code, kofi_payer_email, kofi_last_message_id"

func scanTenant(sc interface{ Scan(...any) error }) (store.Tenant, error) {
	var t store.Tenant
	var created, updated, deleted, expires string
	if err := sc.Scan(&t.ID, &t.Slug, &t.Name, &t.OwnerUserID, &t.Plan, &t.Status, &created, &updated, &deleted, &expires, &t.KofiClaimCode, &t.KofiPayerEmail, &t.KofiLastMessageID); err != nil {
		return t, err
	}
	t.CreatedAt, t.UpdatedAt = parseTime(created), parseTime(updated)
	if deleted != "" {
		d := parseTime(deleted)
		t.DeletedAt = &d
	}
	if expires != "" {
		e := parseTime(expires)
		t.PlanExpiresAt = &e
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
	_, err := d.db.ExecContext(ctx, `INSERT INTO tenants (id, slug, name, owner_user_id, plan, status, created_at, updated_at, plan_expires_at, kofi_claim_code, kofi_payer_email, kofi_last_message_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Slug, t.Name, t.OwnerUserID, t.Plan, t.Status, fmtTime(now), fmtTime(now), fmtTimePtr(t.PlanExpiresAt), t.KofiClaimCode, t.KofiPayerEmail, t.KofiLastMessageID)
	return err
}

func (d *DB) GetTenant(ctx context.Context, id string) (*store.Tenant, error) {
	t, err := scanTenant(d.db.QueryRowContext(ctx, "SELECT "+tenantCols+" FROM tenants WHERE id=?", id))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) GetTenantBySlug(ctx context.Context, slug string) (*store.Tenant, error) {
	t, err := scanTenant(d.db.QueryRowContext(ctx, "SELECT "+tenantCols+" FROM tenants WHERE slug=?", slug))
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
	_, err := d.db.ExecContext(ctx, `UPDATE tenants SET slug=?, name=?, owner_user_id=?, plan=?, status=?, updated_at=?, plan_expires_at=?, kofi_claim_code=?, kofi_payer_email=?, kofi_last_message_id=? WHERE id=?`,
		t.Slug, t.Name, t.OwnerUserID, t.Plan, t.Status, fmtTime(t.UpdatedAt), fmtTimePtr(t.PlanExpiresAt), t.KofiClaimCode, t.KofiPayerEmail, t.KofiLastMessageID, t.ID)
	return err
}

// fmtTimePtr renders an optional timestamp; nil becomes the empty string this
// schema uses for "not set", matching deleted_at.
func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(t.UTC())
}

// --- Tenant invites ---

const inviteCols = "id, tenant_id, email_lower, role, token_hash, expires_at, accepted_at, created_at"

func scanInvite(sc interface{ Scan(...any) error }) (store.TenantInvite, error) {
	var i store.TenantInvite
	var expires, accepted, created string
	if err := sc.Scan(&i.ID, &i.TenantID, &i.Email, &i.Role, &i.TokenHash, &expires, &accepted, &created); err != nil {
		return i, err
	}
	i.ExpiresAt, i.AcceptedAt, i.CreatedAt = parseTime(expires), parseTime(accepted), parseTime(created)
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
	_, err := d.db.ExecContext(ctx, `INSERT INTO tenant_invites (id, tenant_id, email_lower, role, token_hash, expires_at, accepted_at, created_at) VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
		inv.ID, tenantID, inv.Email, inv.Role, inv.TokenHash, fmtTime(inv.ExpiresAt.UTC()), fmtTime(inv.CreatedAt.UTC()))
	return err
}

func (d *DB) GetPendingInviteByEmail(ctx context.Context, email string) (*store.TenantInvite, error) {
	i, err := scanInvite(d.db.QueryRowContext(ctx, "SELECT "+inviteCols+" FROM tenant_invites WHERE email_lower=? AND accepted_at='' AND expires_at > ? ORDER BY created_at DESC LIMIT 1",
		strings.ToLower(strings.TrimSpace(email)), fmtTime(time.Now().UTC())))
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func (d *DB) AcceptInvite(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE tenant_invites SET accepted_at=? WHERE id=? AND accepted_at=''", fmtTime(time.Now().UTC()), id)
	return err
}

func (d *DB) ListInvites(ctx context.Context, tenantID string) ([]store.TenantInvite, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+inviteCols+" FROM tenant_invites WHERE tenant_id=? ORDER BY created_at DESC", tenantID)
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
	_, err := d.db.ExecContext(ctx, "DELETE FROM tenant_invites WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}
