package postgres

import (
	"context"
	"strconv"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// CountBillableDevices counts the devices a tenant provisioned. idx_devices_tenant
// makes this cheap, and counting live avoids the drift a stored counter would
// have: a tenant purge and a bridge changing hands both move rows without
// passing through one place.
func (d *DB) CountBillableDevices(ctx context.Context, tenantID string) (int, error) {
	args := []any{tenantID}
	q := `SELECT count(*) FROM devices WHERE tenant_id = $1`
	if n := len(store.ArtefactDeviceTypes); n > 0 {
		holders := make([]string, n)
		for i, t := range store.ArtefactDeviceTypes {
			holders[i] = "$" + strconv.Itoa(i+2)
			args = append(args, t)
		}
		// The placeholders are generated from a package-level list, never from
		// a caller; the values stay bound.
		q += ` AND type NOT IN (` + strings.Join(holders, ", ") + `)`
	}
	var count int
	err := d.db.QueryRowContext(ctx, q, args...).Scan(&count)
	return count, err
}

// CountBridges counts a tenant's bridges, which share the device ceiling.
func (d *DB) CountBridges(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM bridges WHERE tenant_id = $1`, tenantID).Scan(&n)
	return n, err
}
