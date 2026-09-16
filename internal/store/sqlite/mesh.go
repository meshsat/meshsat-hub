package sqlite

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Mesh node presence (MESHSAT-1181); the Postgres dialect is the reference.
//
// Times are TEXT here, as everywhere in this driver: modernc.org/sqlite refuses
// sql.NullTime against a TEXT column. boothTime/boothParse own the format, and
// time.DateTime sorts lexicographically, so the >= and MAX below compare
// correctly as strings.

// RecordMeshNode notes that nodeID was heard behind bridgeID. last_heard uses
// MAX so a reordered write from the other replica cannot move it backwards.
func (d *DB) RecordMeshNode(ctx context.Context, tenantID, bridgeID, nodeID string, heardAt time.Time) error {
	ts := boothTime(heardAt)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO mesh_nodes (tenant_id, bridge_id, node_id, first_heard, last_heard)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, bridge_id, node_id) DO UPDATE
		   SET last_heard = MAX(mesh_nodes.last_heard, excluded.last_heard)`,
		tenantID, bridgeID, nodeID, ts, ts)
	return err
}

func (d *DB) MeshNodesSeenSince(ctx context.Context, tenantID, bridgeID string, since time.Time) ([]store.MeshNode, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, bridge_id, node_id, first_heard, last_heard
		 FROM mesh_nodes
		 WHERE tenant_id = ? AND bridge_id = ? AND last_heard >= ?
		 ORDER BY last_heard DESC`, tenantID, bridgeID, boothTime(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.MeshNode
	for rows.Next() {
		var n store.MeshNode
		var first, last string
		if err := rows.Scan(&n.TenantID, &n.BridgeID, &n.NodeID, &first, &last); err != nil {
			return nil, err
		}
		n.FirstHeard, n.LastHeard = boothParse(first), boothParse(last)
		out = append(out, n)
	}
	return out, rows.Err()
}
