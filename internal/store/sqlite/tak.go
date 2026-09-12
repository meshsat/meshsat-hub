package sqlite

// Hosted per-tenant TAK (MESHSAT-1037), mirroring internal/store/postgres/tak.go.
//
// Times are TEXT here, and the columns are NOT NULL DEFAULT '', so an absent
// timestamp arrives as a VALID empty string rather than as SQL NULL. The
// neighbouring files do `t, _ := time.Parse(...)` and keep the result, which
// turns an empty string into a pointer to the zero instant -- harmless for a
// "last seen" display, and wrong here: a certificate expiry or an enrollment
// deadline of year 1 reads as a real, long-past deadline. takTime parses
// strictly and leaves the pointer nil instead.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func (d *DB) UpsertTAKInstance(ctx context.Context, inst *store.TAKInstance) error {
	if inst == nil || inst.TenantID == "" {
		return errors.New("sqlite: TAK instance needs a tenant id")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tak_instances (tenant_id, label, state, phase, host, ca_cert_pem, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT (tenant_id) DO UPDATE SET
			label = excluded.label,
			state = excluded.state,
			phase = excluded.phase,
			host = excluded.host,
			ca_cert_pem = excluded.ca_cert_pem,
			updated_at = datetime('now')`,
		inst.TenantID, inst.Label, inst.State, inst.Phase, inst.Host, inst.CACertPEM)
	return err
}

func (d *DB) GetTAKInstance(ctx context.Context, tenantID string) (*store.TAKInstance, error) {
	var inst store.TAKInstance
	var createdAt, updatedAt sql.NullString
	err := d.db.QueryRowContext(ctx, `
		SELECT tenant_id, label, state, phase, host, ca_cert_pem, created_at, updated_at
		FROM tak_instances WHERE tenant_id = ?`, tenantID).Scan(
		&inst.TenantID, &inst.Label, &inst.State, &inst.Phase, &inst.Host,
		&inst.CACertPEM, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	inst.CreatedAt = takTime(createdAt)
	inst.UpdatedAt = takTime(updatedAt)
	return &inst, nil
}

// ListTAKInstances returns every tenant's instance: the Hub builds one takfront
// directory covering all of them, because a single listener has to resolve any
// phone's certificate issuer.
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
		var createdAt, updatedAt sql.NullString
		if err := rows.Scan(&inst.TenantID, &inst.Label, &inst.State, &inst.Phase,
			&inst.Host, &inst.CACertPEM, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		inst.CreatedAt = takTime(createdAt)
		inst.UpdatedAt = takTime(updatedAt)
		out = append(out, &inst)
	}
	return out, rows.Err()
}

func (d *DB) DeleteTAKInstance(ctx context.Context, tenantID string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM tak_instances WHERE tenant_id = ?`, tenantID)
	return err
}

func (d *DB) CreateTAKUser(ctx context.Context, tenantID string, u *store.TAKUser) error {
	if u == nil || u.Username == "" {
		return errors.New("sqlite: TAK user needs a username")
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tak_users (tenant_id, username, callsign, active, cert_serial,
			cert_not_after, revoked_serial, enroll_token_hash, enroll_expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		tenantID, u.Username, u.Callsign, u.Active, u.CertSerial,
		takTimeText(u.CertNotAfter), u.RevokedSerial, u.EnrollTokenHash,
		takTimeText(u.EnrollExpiresAt))
	return err
}

func (d *DB) GetTAKUser(ctx context.Context, tenantID string, username string) (*store.TAKUser, error) {
	u, err := scanTAKUser(d.db.QueryRowContext(ctx, `
		SELECT tenant_id, username, callsign, active, cert_serial, cert_not_after,
			revoked_serial, enroll_token_hash, enroll_expires_at, created_at, updated_at
		FROM tak_users WHERE tenant_id = ? AND username = ?`, tenantID, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return u, err
}

func (d *DB) ListTAKUsers(ctx context.Context, tenantID string) ([]*store.TAKUser, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT tenant_id, username, callsign, active, cert_serial, cert_not_after,
			revoked_serial, enroll_token_hash, enroll_expires_at, created_at, updated_at
		FROM tak_users WHERE tenant_id = ? ORDER BY username`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*store.TAKUser
	for rows.Next() {
		u, err := scanTAKUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateTAKUser rewrites the mutable columns. Username is the key and never
// changes: OpenTAKServer matches a certificate's common name against it, so a
// rename would orphan every certificate already issued to that person.
func (d *DB) UpdateTAKUser(ctx context.Context, tenantID string, u *store.TAKUser) error {
	if u == nil || u.Username == "" {
		return errors.New("sqlite: TAK user needs a username")
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE tak_users SET callsign = ?, active = ?, cert_serial = ?,
			cert_not_after = ?, revoked_serial = ?, enroll_token_hash = ?,
			enroll_expires_at = ?, updated_at = datetime('now')
		WHERE tenant_id = ? AND username = ?`,
		u.Callsign, u.Active, u.CertSerial, takTimeText(u.CertNotAfter),
		u.RevokedSerial, u.EnrollTokenHash, takTimeText(u.EnrollExpiresAt),
		tenantID, u.Username)
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
		`DELETE FROM tak_users WHERE tenant_id = ? AND username = ?`, tenantID, username)
	return err
}

// CountTAKUsers is the TAK meter, counted live like the device counters.
func (d *DB) CountTAKUsers(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tak_users WHERE tenant_id = ?`, tenantID).Scan(&n)
	return n, err
}

type takRowScanner interface {
	Scan(dest ...any) error
}

func scanTAKUser(s takRowScanner) (*store.TAKUser, error) {
	var u store.TAKUser
	var certNotAfter, enrollExpires, createdAt, updatedAt sql.NullString
	if err := s.Scan(&u.TenantID, &u.Username, &u.Callsign, &u.Active, &u.CertSerial,
		&certNotAfter, &u.RevokedSerial, &u.EnrollTokenHash, &enrollExpires,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	u.CertNotAfter = takTime(certNotAfter)
	u.EnrollExpiresAt = takTime(enrollExpires)
	u.CreatedAt = takTime(createdAt)
	u.UpdatedAt = takTime(updatedAt)
	return &u, nil
}

// takTime parses a stored timestamp, returning nil for an absent one.
//
// Deliberately stricter than the neighbouring files: these columns are NOT NULL
// an empty-string default, so "no value" is a zero-length string that
// time.Parse rejects rather than a SQL NULL. Ignoring
// that error and keeping the zero instant would turn "never enrolled" into "the
// enrollment window closed in year 1", which reads as a real expired deadline.
func takTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.DateTime, s.String)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// takTimeText is the inverse: an absent time is stored as the empty string the
// column defaults to, never as the zero instant.
func takTimeText(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateTime)
}
