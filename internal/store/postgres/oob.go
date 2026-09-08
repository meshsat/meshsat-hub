package postgres

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

const oobColumns = "tenant_id, bridge_id, peer_id, key_enc, local_role, phone, sat_imei, tx_counter, rx_high, rx_window, enabled, created_at, updated_at"

func scanOOBPeer(s rowScanner) (*store.OOBPeer, error) {
	var p store.OOBPeer
	if err := s.Scan(&p.TenantID, &p.BridgeID, &p.PeerID, &p.KeyEnc, &p.LocalRole, &p.Phone, &p.SatIMEI, &p.TxCounter, &p.RxHigh, &p.RxWindow, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = utc(p.CreatedAt), utc(p.UpdatedAt)
	return &p, nil
}

func (d *DB) UpsertOOBPeer(ctx context.Context, p *store.OOBPeer) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := d.db.ExecContext(ctx, `INSERT INTO bridge_oob_peers (`+oobColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id, bridge_id) DO UPDATE SET peer_id = EXCLUDED.peer_id, key_enc = EXCLUDED.key_enc,
		local_role = EXCLUDED.local_role, phone = EXCLUDED.phone, sat_imei = EXCLUDED.sat_imei,
		tx_counter = EXCLUDED.tx_counter, rx_high = EXCLUDED.rx_high, rx_window = EXCLUDED.rx_window,
		enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`,
		p.TenantID, p.BridgeID, p.PeerID, p.KeyEnc, p.LocalRole, p.Phone, p.SatIMEI, p.TxCounter, p.RxHigh, p.RxWindow, p.Enabled, p.CreatedAt, p.UpdatedAt)
	return err
}

func (d *DB) GetOOBPeer(ctx context.Context, tenantID string, bridgeID string) (*store.OOBPeer, error) {
	return scanOOBPeer(d.db.QueryRowContext(ctx, "SELECT "+oobColumns+" FROM bridge_oob_peers WHERE tenant_id=$1 AND bridge_id=$2", tenantID, bridgeID))
}

func (d *DB) ListOOBPeersByPeerID(ctx context.Context, peerID int) ([]store.OOBPeer, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+oobColumns+" FROM bridge_oob_peers WHERE peer_id=$1 AND enabled ORDER BY tenant_id, bridge_id", peerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.OOBPeer
	for rows.Next() {
		p, err := scanOOBPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (d *DB) DeleteOOBPeer(ctx context.Context, tenantID string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM bridge_oob_peers WHERE tenant_id=$1 AND bridge_id=$2", tenantID, bridgeID)
	return err
}

func (d *DB) NextOOBCounter(ctx context.Context, tenantID string, bridgeID string) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx, "UPDATE bridge_oob_peers SET tx_counter = tx_counter + 1, updated_at = now() WHERE tenant_id=$1 AND bridge_id=$2 RETURNING tx_counter", tenantID, bridgeID).Scan(&n)
	return n, err
}

func (d *DB) SetOOBReplayWindow(ctx context.Context, tenantID string, bridgeID string, high int64, window int64) error {
	_, err := d.db.ExecContext(ctx, "UPDATE bridge_oob_peers SET rx_high=$1, rx_window=$2, updated_at=now() WHERE tenant_id=$3 AND bridge_id=$4", high, window, tenantID, bridgeID)
	return err
}
