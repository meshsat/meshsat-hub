package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

const oobColumns = "tenant_id, bridge_id, peer_id, key_enc, local_role, phone, sat_imei, tx_counter, rx_high, rx_window, enabled, created_at, updated_at"

type oobScanner interface{ Scan(dest ...any) error }

func scanOOBPeer(s oobScanner) (*store.OOBPeer, error) {
	var p store.OOBPeer
	var enabled int
	var created, updated string
	if err := s.Scan(&p.TenantID, &p.BridgeID, &p.PeerID, &p.KeyEnc, &p.LocalRole, &p.Phone, &p.SatIMEI, &p.TxCounter, &p.RxHigh, &p.RxWindow, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return &p, nil
}

func (d *DB) UpsertOOBPeer(ctx context.Context, p *store.OOBPeer) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := d.db.ExecContext(ctx, `INSERT INTO bridge_oob_peers (`+oobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, bridge_id) DO UPDATE SET peer_id = excluded.peer_id, key_enc = excluded.key_enc, local_role = excluded.local_role,
		phone = excluded.phone, sat_imei = excluded.sat_imei, tx_counter = excluded.tx_counter, rx_high = excluded.rx_high, rx_window = excluded.rx_window,
		enabled = excluded.enabled, updated_at = excluded.updated_at`,
		p.TenantID, p.BridgeID, p.PeerID, p.KeyEnc, p.LocalRole, p.Phone, p.SatIMEI, p.TxCounter, p.RxHigh, p.RxWindow, boolToInt(p.Enabled),
		p.CreatedAt.Format(time.DateTime), p.UpdatedAt.Format(time.DateTime))
	return err
}

func (d *DB) GetOOBPeer(ctx context.Context, tenantID string, bridgeID string) (*store.OOBPeer, error) {
	return scanOOBPeer(d.db.QueryRowContext(ctx, "SELECT "+oobColumns+" FROM bridge_oob_peers WHERE tenant_id=? AND bridge_id=?", tenantID, bridgeID))
}

func (d *DB) ListOOBPeersByPeerID(ctx context.Context, peerID int) ([]store.OOBPeer, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+oobColumns+" FROM bridge_oob_peers WHERE peer_id=? AND enabled=1 ORDER BY tenant_id, bridge_id", peerID)
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
	_, err := d.db.ExecContext(ctx, "DELETE FROM bridge_oob_peers WHERE tenant_id=? AND bridge_id=?", tenantID, bridgeID)
	return err
}

func (d *DB) NextOOBCounter(ctx context.Context, tenantID string, bridgeID string) (int64, error) {
	var c int64
	err := d.db.QueryRowContext(ctx, "UPDATE bridge_oob_peers SET tx_counter = tx_counter + 1, updated_at = datetime('now') WHERE tenant_id=? AND bridge_id=? RETURNING tx_counter", tenantID, bridgeID).Scan(&c)
	if err == sql.ErrNoRows {
		return 0, sql.ErrNoRows
	}
	return c, err
}

func (d *DB) SetOOBReplayWindow(ctx context.Context, tenantID string, bridgeID string, high int64, window int64) error {
	_, err := d.db.ExecContext(ctx, "UPDATE bridge_oob_peers SET rx_high=?, rx_window=?, updated_at=datetime('now') WHERE tenant_id=? AND bridge_id=?", high, window, tenantID, bridgeID)
	return err
}
