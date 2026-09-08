package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Tenant offboarding, mirroring the Postgres implementation. The set of
// tenant-scoped tables is read from SQLite's own catalogue for the same reason
// it is there: a list written down here would go stale the first time someone
// adds a table and forgets this file, and an erasure that quietly misses a
// table is worse than no erasure at all.

func (d *DB) tenantTables(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT m.name FROM sqlite_master m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.name = 'tenant_id'
		ORDER BY m.name`)
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

func (d *DB) SoftDeleteTenant(ctx context.Context, id string, at time.Time) error {
	if id == store.DefaultTenantID {
		return store.ErrReservedTenantID
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tenants SET status=?, deleted_at=?, updated_at=? WHERE id=?`,
		store.TenantDeleted, fmtTime(at.UTC()), fmtTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) ListTenantsDeletedBefore(ctx context.Context, cutoff time.Time) ([]store.Tenant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+tenantCols+` FROM tenants WHERE status=? AND deleted_at<>'' AND deleted_at<? ORDER BY deleted_at`,
		store.TenantDeleted, fmtTime(cutoff.UTC()))
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
		// #nosec G202 -- identifier from sqlite_master, quoted
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(t)+` WHERE tenant_id = ?`, id); err != nil {
			return fmt.Errorf("purge %s: %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("purge tenants: %w", err)
	}
	return tx.Commit()
}

func (d *DB) ExportTenant(ctx context.Context, id string) (map[string][]map[string]any, error) {
	tables, err := d.tenantTables(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenant tables: %w", err)
	}
	out := map[string][]map[string]any{}
	for _, t := range tables {
		// #nosec G202 -- identifier from sqlite_master, quoted
		rows, err := d.db.QueryContext(ctx, `SELECT * FROM `+quoteIdent(t)+` WHERE tenant_id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t, err)
		}
		recs, err := scanAll(rows)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t, err)
		}
		out[t] = recs
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = ?`, id)
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

// scanAll turns a result set into plain maps, closing the rows. Kept beside
// the Postgres twin rather than shared: the two store packages deliberately
// do not depend on each other.
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
