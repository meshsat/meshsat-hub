package postgres

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Mesh node presence (MESHSAT-1181). See store.MeshNode for why a row proves
// something and a missing row proves nothing.

// RecordMeshNode notes that nodeID was heard behind bridgeID.
//
// last_heard uses GREATEST rather than assignment: both replicas receive every
// mesh message and may write in either order, and a reordered pair must not
// move a node's last_heard backwards in time.
func (d *DB) RecordMeshNode(ctx context.Context, tenantID, bridgeID, nodeID string, heardAt time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO mesh_nodes (tenant_id, bridge_id, node_id, first_heard, last_heard)
		 VALUES ($1, $2, $3, $4, $4)
		 ON CONFLICT (tenant_id, bridge_id, node_id) DO UPDATE
		   SET last_heard = GREATEST(mesh_nodes.last_heard, EXCLUDED.last_heard)`,
		tenantID, bridgeID, nodeID, heardAt.UTC())
	return err
}

func (d *DB) MeshNodesSeenSince(ctx context.Context, tenantID, bridgeID string, since time.Time) ([]store.MeshNode, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, bridge_id, node_id, first_heard, last_heard
		 FROM mesh_nodes
		 WHERE tenant_id = $1 AND bridge_id = $2 AND last_heard >= $3
		 ORDER BY last_heard DESC`, tenantID, bridgeID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.MeshNode
	for rows.Next() {
		var n store.MeshNode
		if err := rows.Scan(&n.TenantID, &n.BridgeID, &n.NodeID, &n.FirstHeard, &n.LastHeard); err != nil {
			return nil, err
		}
		n.FirstHeard, n.LastHeard = utc(n.FirstHeard), utc(n.LastHeard)
		out = append(out, n)
	}
	return out, rows.Err()
}
