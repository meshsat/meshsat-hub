package sqlite

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Email PGP contacts (MESHSAT-1123). See the Postgres twin: keyed on
// (tenant_id, email) so one tenant's key for an address cannot replace another's.

func (d *DB) SaveEmailContact(ctx context.Context, tenantID, email, armoredKey string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO email_contacts (tenant_id, email, armored_key, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT (tenant_id, email) DO UPDATE SET armored_key=excluded.armored_key, updated_at=datetime('now')`,
		tenantID, email, armoredKey)
	return err
}

func (d *DB) ListEmailContacts(ctx context.Context, tenantID string) ([]store.EmailContact, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, email, armored_key, created_at, updated_at
		 FROM email_contacts WHERE tenant_id = ? ORDER BY email`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.EmailContact
	for rows.Next() {
		var c store.EmailContact
		var created, updated string
		if err := rows.Scan(&c.TenantID, &c.Email, &c.ArmoredKey, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) DeleteEmailContact(ctx context.Context, tenantID, email string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM email_contacts WHERE tenant_id = ? AND email = ?`, tenantID, email)
	return err
}
