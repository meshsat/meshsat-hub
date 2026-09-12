package postgres

// Hosted per-tenant TAK (MESHSAT-1037).
//
// Two tables, both carrying tenant_id, which is what makes them appear in the
// tenant export and the purge: those find their tables by reflecting on that
// column (tenant_lifecycle.go), never from a list in code. A table without it is
// silently never erased.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// UpsertTAKInstance records or updates the Hub's view of a tenant's instance.
//
// One row per tenant, so the tenant id is the primary key and this is an upsert
// rather than an insert: the operator's reconcile loop reports the same instance
// repeatedly as its phase changes, and each report should land on the same row.
func (d *DB) UpsertTAKInstance(ctx context.Context, inst *store.TAKInstance) error {
	if inst == nil || inst.TenantID == "" {
		return errors.New("postgres: TAK instance needs a tenant id")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tak_instances (tenant_id, label, state, phase, host, ca_cert_pem, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (tenant_id) DO UPDATE SET
			label = EXCLUDED.label,
			state = EXCLUDED.state,
			phase = EXCLUDED.phase,
			host = EXCLUDED.host,
			ca_cert_pem = EXCLUDED.ca_cert_pem,
			updated_at = now()`,
		inst.TenantID, inst.Label, inst.State, inst.Phase, inst.Host, inst.CACertPEM)
	return err
}

func (d *DB) GetTAKInstance(ctx context.Context, tenantID string) (*store.TAKInstance, error) {
	var inst store.TAKInstance
	var createdAt, updatedAt sql.NullTime
	err := d.db.QueryRowContext(ctx, `
		SELECT tenant_id, label, state, phase, host, ca_cert_pem, created_at, updated_at
		FROM tak_instances WHERE tenant_id = $1`, tenantID).Scan(
		&inst.TenantID, &inst.Label, &inst.State, &inst.Phase, &inst.Host,
		&inst.CACertPEM, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inst.CreatedAt = utcPtr(createdAt)
	inst.UpdatedAt = utcPtr(updatedAt)
	return &inst, nil
}

// ListTAKInstances returns every tenant's instance. Not tenant-scoped on
// purpose: the Hub builds one takfront directory covering all tenants, because a
// single listener has to resolve any phone's certificate issuer.
func (d *DB) ListTAKInstances(ctx context.Context) ([]*store.TAKInstance, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT tenant_id, label, state, phase, host, ca_cert_pem, created_at, updated_at
		FROM tak_instances ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*store.TAKInstance
	for rows.Next() {
		var inst store.TAKInstance
		var createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&inst.TenantID, &inst.Label, &inst.State, &inst.Phase,
			&inst.Host, &inst.CACertPEM, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		inst.CreatedAt = utcPtr(createdAt)
		inst.UpdatedAt = utcPtr(updatedAt)
		out = append(out, &inst)
	}
	return out, rows.Err()
}

func (d *DB) DeleteTAKInstance(ctx context.Context, tenantID string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM tak_instances WHERE tenant_id = $1`, tenantID)
	return err
}

// CreateTAKUser adds one account. The primary key is (tenant_id, username), so a
// duplicate is refused by the database rather than by a read-then-write that two
// replicas could both pass.
func (d *DB) CreateTAKUser(ctx context.Context, tenantID string, u *store.TAKUser) error {
	if u == nil || u.Username == "" {
		return errors.New("postgres: TAK user needs a username")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tak_users (tenant_id, username, callsign, active, cert_serial,
			cert_not_after, revoked_serial, enroll_token_hash, enroll_expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
		tenantID, u.Username, u.Callsign, u.Active, u.CertSerial,
		nullTime(u.CertNotAfter), u.RevokedSerial, u.EnrollTokenHash, nullTime(u.EnrollExpiresAt))
	return err
}

func (d *DB) GetTAKUser(ctx context.Context, tenantID string, username string) (*store.TAKUser, error) {
	u, err := d.scanTAKUser(d.db.QueryRowContext(ctx, `
		SELECT tenant_id, username, callsign, active, cert_serial, cert_not_after,
			revoked_serial, enroll_token_hash, enroll_expires_at, created_at, updated_at
		FROM tak_users WHERE tenant_id = $1 AND username = $2`, tenantID, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return u, err
}

func (d *DB) ListTAKUsers(ctx context.Context, tenantID string) ([]*store.TAKUser, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT tenant_id, username, callsign, active, cert_serial, cert_not_after,
			revoked_serial, enroll_token_hash, enroll_expires_at, created_at, updated_at
		FROM tak_users WHERE tenant_id = $1 ORDER BY username`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*store.TAKUser
	for rows.Next() {
		u, err := d.scanTAKUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateTAKUser rewrites the mutable columns. Username is the key and is never
// changed: OpenTAKServer matches a certificate's common name against it, so
// renaming would orphan every certificate already issued to that person.
func (d *DB) UpdateTAKUser(ctx context.Context, tenantID string, u *store.TAKUser) error {
	if u == nil || u.Username == "" {
		return errors.New("postgres: TAK user needs a username")
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE tak_users SET callsign = $3, active = $4, cert_serial = $5,
			cert_not_after = $6, revoked_serial = $7, enroll_token_hash = $8,
			enroll_expires_at = $9, updated_at = now()
		WHERE tenant_id = $1 AND username = $2`,
		tenantID, u.Username, u.Callsign, u.Active, u.CertSerial,
		nullTime(u.CertNotAfter), u.RevokedSerial, u.EnrollTokenHash, nullTime(u.EnrollExpiresAt))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) DeleteTAKUser(ctx context.Context, tenantID string, username string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM tak_users WHERE tenant_id = $1 AND username = $2`, tenantID, username)
	return err
}

// CountTAKUsers is the TAK meter. Counted live, like the device counters: a
// purge and a user removal both move rows without passing through one place, so
// a stored counter would drift.
func (d *DB) CountTAKUsers(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tak_users WHERE tenant_id = $1`, tenantID).Scan(&n)
	return n, err
}

// scanTAKUser serves both Get and List. It takes the package's existing
// rowScanner (bridges.go), which *sql.Row and *sql.Rows both satisfy; declaring
// a second identical interface here was a redeclaration the compiler refused.
func (d *DB) scanTAKUser(s rowScanner) (*store.TAKUser, error) {
	var u store.TAKUser
	var certNotAfter, enrollExpires, createdAt, updatedAt sql.NullTime
	if err := s.Scan(&u.TenantID, &u.Username, &u.Callsign, &u.Active, &u.CertSerial,
		&certNotAfter, &u.RevokedSerial, &u.EnrollTokenHash, &enrollExpires,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	u.CertNotAfter = utcPtr(certNotAfter)
	u.EnrollExpiresAt = utcPtr(enrollExpires)
	u.CreatedAt = utcPtr(createdAt)
	u.UpdatedAt = utcPtr(updatedAt)
	return &u, nil
}

// nullTime turns an absent time into SQL NULL rather than the zero instant,
// which would otherwise be stored as year 1 and read back as a real deadline.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
