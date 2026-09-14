package postgres

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Email PGP contacts (MESHSAT-1123). Tenant-scoped throughout, and the upsert is
// keyed on (tenant_id, email) rather than email: two tenants may correspond with
// the same address, and a conflict on email alone would let one tenant's key
// replace another's -- which is the defect the in-memory map actually had.

func (d *DB) SaveEmailContact(ctx context.Context, tenantID, email, armoredKey string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO email_contacts (tenant_id, email, armored_key, updated_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (tenant_id, email) DO UPDATE SET armored_key=EXCLUDED.armored_key, updated_at=now()`,
		tenantID, email, armoredKey)
	return err
}

func (d *DB) ListEmailContacts(ctx context.Context, tenantID string) ([]store.EmailContact, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, email, armored_key, created_at, updated_at
		 FROM email_contacts WHERE tenant_id = $1 ORDER BY email`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.EmailContact
	for rows.Next() {
		var c store.EmailContact
		if err := rows.Scan(&c.TenantID, &c.Email, &c.ArmoredKey, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = utc(c.CreatedAt), utc(c.UpdatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) DeleteEmailContact(ctx context.Context, tenantID, email string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM email_contacts WHERE tenant_id = $1 AND email = $2`, tenantID, email)
	return err
}
