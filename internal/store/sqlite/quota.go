package sqlite

import (
	"context"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// CountBillableDevices counts the devices a tenant provisioned, excluding the
// artefact types an integration creates on its own. Mirrors the Postgres
// implementation; the conformance suite holds both to the same behaviour.
func (d *DB) CountBillableDevices(ctx context.Context, tenantID string) (int, error) {
	args := []any{tenantID}
	q := `SELECT count(*) FROM devices WHERE tenant_id = ?`
	if n := len(store.ArtefactDeviceTypes); n > 0 {
		for _, t := range store.ArtefactDeviceTypes {
			args = append(args, t)
		}
		q += ` AND type NOT IN (` + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)`
	}
	var count int
	err := d.db.QueryRowContext(ctx, q, args...).Scan(&count)
	return count, err
}

// CountBridges counts a tenant's bridges, which share the device ceiling.
func (d *DB) CountBridges(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM bridges WHERE tenant_id = ?`, tenantID).Scan(&n)
	return n, err
}
