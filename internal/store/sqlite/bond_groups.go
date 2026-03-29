package sqlite

import (
	"context"
	"fmt"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func (d *DB) CreateBondGroup(ctx context.Context, tenantID, bridgeID string, g *store.BondGroup) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO bond_groups (id, tenant_id, bridge_id, label, members, cost_budget) VALUES (?, ?, ?, ?, ?, ?)`,
		g.ID, tenantID, bridgeID, g.Label, g.Members, g.CostBudget,
	)
	if err != nil {
		return fmt.Errorf("insert bond group: %w", err)
	}
	return nil
}

func (d *DB) GetBondGroups(ctx context.Context, tenantID, bridgeID string) ([]store.BondGroup, error) {
	var groups []store.BondGroup
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, tenant_id, bridge_id, label, members, cost_budget, created_at FROM bond_groups WHERE tenant_id = ? AND bridge_id = ? ORDER BY created_at ASC`,
		tenantID, bridgeID,
	)
	if err != nil {
		return nil, fmt.Errorf("query bond groups: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var g store.BondGroup
		if err := rows.Scan(&g.ID, &g.TenantID, &g.BridgeID, &g.Label, &g.Members, &g.CostBudget, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan bond group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (d *DB) DeleteBondGroup(ctx context.Context, tenantID, bridgeID, groupID string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM bond_groups WHERE tenant_id = ? AND bridge_id = ? AND id = ?`,
		tenantID, bridgeID, groupID,
	)
	return err
}
