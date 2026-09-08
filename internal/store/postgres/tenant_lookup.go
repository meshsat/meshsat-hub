package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// LookupDeviceTenant implements store.Store.
func (d *DB) LookupDeviceTenant(ctx context.Context, imei string) (string, error) {
	return lookupTenant(ctx, d.db, "SELECT tenant_id FROM devices WHERE imei=%s", imei)
}

// LookupBridgeTenant implements store.Store.
func (d *DB) LookupBridgeTenant(ctx context.Context, bridgeID string) (string, error) {
	return lookupTenant(ctx, d.db, "SELECT tenant_id FROM bridges WHERE bridge_id=%s", bridgeID)
}

// lookupTenant runs a single-column query and maps 0 rows to store.ErrNotFound
// and >1 distinct tenants to store.ErrAmbiguousTenant.
func lookupTenant(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, q, key string) (string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(q, "$1"), key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	tenant := ""
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", err
		}
		if tenant != "" && t != tenant {
			return "", store.ErrAmbiguousTenant
		}
		tenant = t
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if tenant == "" {
		return "", store.ErrNotFound
	}
	return tenant, nil
}
