package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Federation peers and grants (MESHSAT-1549) ---

const federationPeerCols = "tenant_id, id, name, relay_url, cert_pem, fingerprint, created_by_email, created_at"

func scanFederationPeer(sc interface{ Scan(...any) error }) (*store.FederationPeer, error) {
	var p store.FederationPeer
	var created string
	if err := sc.Scan(&p.TenantID, &p.ID, &p.Name, &p.RelayURL, &p.CertPEM, &p.Fingerprint, &p.CreatedByEmail, &created); err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(created)
	return &p, nil
}

func (d *DB) CreateFederationPeer(ctx context.Context, tenantID string, p *store.FederationPeer) error {
	if p.ID == "" {
		p.ID = xid.New().String()
	}
	p.TenantID = tenantID
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO federation_peers (tenant_id, id, name, relay_url, cert_pem, fingerprint, created_by_email, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		tenantID, p.ID, p.Name, p.RelayURL, p.CertPEM, p.Fingerprint, p.CreatedByEmail, fmtTime(p.CreatedAt.UTC()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListFederationPeers(ctx context.Context, tenantID string) ([]store.FederationPeer, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+federationPeerCols+` FROM federation_peers WHERE tenant_id=? ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.FederationPeer{}
	for rows.Next() {
		p, err := scanFederationPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (d *DB) GetFederationPeer(ctx context.Context, tenantID, id string) (*store.FederationPeer, error) {
	p, err := scanFederationPeer(d.db.QueryRowContext(ctx, `SELECT `+federationPeerCols+` FROM federation_peers WHERE tenant_id=? AND id=?`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (d *DB) GetFederationPeerByFingerprint(ctx context.Context, tenantID, fingerprint string) (*store.FederationPeer, error) {
	p, err := scanFederationPeer(d.db.QueryRowContext(ctx, `SELECT `+federationPeerCols+` FROM federation_peers WHERE tenant_id=? AND fingerprint=?`, tenantID, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (d *DB) DeleteFederationPeer(ctx context.Context, tenantID, id string) error {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_grants WHERE tenant_id=? AND peer_id=?`, tenantID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM federation_peers WHERE tenant_id=? AND id=?`, tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
}

const federationGrantCols = "tenant_id, id, peer_id, from_imei, to_imei, created_by_email, created_at"

func (d *DB) CreateFederationGrant(ctx context.Context, tenantID string, g *store.FederationGrant) error {
	if g.ID == "" {
		g.ID = xid.New().String()
	}
	g.TenantID = tenantID
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO federation_grants (tenant_id, id, peer_id, from_imei, to_imei, created_by_email, created_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		tenantID, g.ID, g.PeerID, g.FromIMEI, g.ToIMEI, g.CreatedByEmail, fmtTime(g.CreatedAt.UTC()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListFederationGrants(ctx context.Context, tenantID string) ([]store.FederationGrant, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+federationGrantCols+` FROM federation_grants WHERE tenant_id=? ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.FederationGrant{}
	for rows.Next() {
		var g store.FederationGrant
		var created string
		if err := rows.Scan(&g.TenantID, &g.ID, &g.PeerID, &g.FromIMEI, &g.ToIMEI, &g.CreatedByEmail, &created); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (d *DB) DeleteFederationGrant(ctx context.Context, tenantID, id string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM federation_grants WHERE tenant_id=? AND id=?`, tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) HasFederationGrant(ctx context.Context, tenantID, peerID, fromIMEI, toIMEI string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_grants WHERE tenant_id=? AND peer_id=? AND from_imei=? AND to_imei=?`,
		tenantID, peerID, fromIMEI, toIMEI).Scan(&n)
	return n > 0, err
}
