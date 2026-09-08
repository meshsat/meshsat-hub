package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Tenant offboarding: export, soft delete, and the purge that eventually
// follows. Deleting a tenant has to reach every row it owns, and the set of
// tables that carry tenant data grows with every migration, so the list is
// read from the catalogue rather than written down here. A hardcoded list is
// one forgotten migration away from an erasure that quietly leaves data
// behind, which is worse than not offering erasure at all.

// tenantTables returns every table with a tenant_id column, alphabetically so
// the work is deterministic and diffable.
func (d *DB) tenantTables(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'tenant_id'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SoftDeleteTenant blocks the tenant and starts its grace period.
func (d *DB) SoftDeleteTenant(ctx context.Context, id string, at time.Time) error {
	if id == store.DefaultTenantID {
		return store.ErrReservedTenantID
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tenants SET status = $1, deleted_at = $2, updated_at = $2 WHERE id = $3`,
		store.TenantDeleted, at.UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListTenantsDeletedBefore returns the tenants whose grace period has run out.
func (d *DB) ListTenantsDeletedBefore(ctx context.Context, cutoff time.Time) ([]store.Tenant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+tenantCols+` FROM tenants WHERE status = $1 AND deleted_at IS NOT NULL AND deleted_at < $2 ORDER BY deleted_at`,
		store.TenantDeleted, cutoff.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.Tenant{}
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PurgeTenant destroys everything the tenant owns. One transaction, so a
// failure part-way leaves the tenant intact and the job can try again rather
// than leaving a half-erased account behind. The schema has no foreign keys
// between these tables, so the order does not matter.
func (d *DB) PurgeTenant(ctx context.Context, id string) error {
	if id == store.DefaultTenantID {
		return store.ErrReservedTenantID
	}
	tables, err := d.tenantTables(ctx)
	if err != nil {
		return fmt.Errorf("list tenant tables: %w", err)
	}
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range tables {
		// t is a table name read from the catalogue, never from a caller, and
		// it is quoted. The tenant id stays a bound parameter.
		// #nosec G202 -- identifier from information_schema, quoted
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(t)+` WHERE tenant_id = $1`, id); err != nil {
			return fmt.Errorf("purge %s: %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenants WHERE id = $1`, id); err != nil {
		return fmt.Errorf("purge tenants: %w", err)
	}
	return tx.Commit()
}

// ExportTenant returns every row the tenant owns, keyed by table. Column names
// come from the driver, so a table added later exports without being named
// here, the same way it is purged without being named here.
func (d *DB) ExportTenant(ctx context.Context, id string) (map[string][]map[string]any, error) {
	tables, err := d.tenantTables(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenant tables: %w", err)
	}
	out := map[string][]map[string]any{}
	for _, t := range tables {
		// #nosec G202 -- identifier from information_schema, quoted
		rows, err := d.db.QueryContext(ctx, `SELECT * FROM `+quoteIdent(t)+` WHERE tenant_id = $1`, id)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t, err)
		}
		recs, err := scanAll(rows)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t, err)
		}
		out[t] = recs
	}
	// The tenant row itself is not tenant_id-scoped.
	rows, err := d.db.QueryContext(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	recs, err := scanAll(rows)
	if err != nil {
		return nil, err
	}
	out["tenants"] = recs
	return out, nil
}

// scanAll turns a result set into plain maps, closing the rows.
func scanAll(rows *sql.Rows) ([]map[string]any, error) {
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make(map[string]any, len(cols))
		for i, c := range cols {
			// Bytes would marshal as base64 and read as noise in an export a
			// person is meant to be able to open.
			if b, ok := vals[i].([]byte); ok {
				rec[c] = string(b)
			} else {
				rec[c] = vals[i]
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// quoteIdent quotes a catalogue-derived identifier. These never come from a
// caller, but an unquoted identifier in a built statement is the shape of a
// future injection, so it does not get to start.
func quoteIdent(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}
