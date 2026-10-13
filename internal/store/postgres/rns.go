package postgres

import (
	"context"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Reticulum path table (MESHSAT-1555) ---

const rnsPathCols = "dest_hash, next_hop, hops, iface, expires_at, random_blobs, announce_raw, updated_at"

func (d *DB) UpsertRNSPath(ctx context.Context, p *store.RNSPath) error {
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = time.Now().UTC()
	}
	// A nil slice would bind as NULL; the columns are NOT NULL and a path
	// with no blobs is a legitimate row.
	blobs, raw := p.RandomBlobs, p.AnnounceRaw
	if blobs == nil {
		blobs = []byte{}
	}
	if raw == nil {
		raw = []byte{}
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO rns_paths (`+rnsPathCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (dest_hash) DO UPDATE SET next_hop=EXCLUDED.next_hop, hops=EXCLUDED.hops, iface=EXCLUDED.iface,
		expires_at=EXCLUDED.expires_at, random_blobs=EXCLUDED.random_blobs, announce_raw=EXCLUDED.announce_raw, updated_at=EXCLUDED.updated_at`,
		p.DestHash, p.NextHop, p.Hops, p.Iface, p.ExpiresAt.UTC(), blobs, raw, p.UpdatedAt.UTC())
	return err
}

func (d *DB) ListRNSPaths(ctx context.Context) ([]store.RNSPath, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+rnsPathCols+` FROM rns_paths ORDER BY dest_hash`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.RNSPath{}
	for rows.Next() {
		var p store.RNSPath
		if err := rows.Scan(&p.DestHash, &p.NextHop, &p.Hops, &p.Iface, &p.ExpiresAt, &p.RandomBlobs, &p.AnnounceRaw, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.ExpiresAt, p.UpdatedAt = utc(p.ExpiresAt), utc(p.UpdatedAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) DeleteRNSPath(ctx context.Context, destHash string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM rns_paths WHERE dest_hash=$1`, destHash)
	return err
}
