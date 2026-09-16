package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The TTC booth flow (MESHSAT-1175), tenant-scoped throughout.
//
// The reason any of this is in the database rather than in a map on the handler:
// both Hub replicas process every message, so a reply coming back off the mesh
// routinely lands on the pod that did not send the original. See store.BoothRelay.

func (d *DB) GetBoothSession(ctx context.Context, tenantID, sender, channel string) (*store.BoothSession, error) {
	var s store.BoothSession
	var optedIn sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, sender, channel, state, opted_in_at, created_at, updated_at
		 FROM booth_sessions WHERE tenant_id = $1 AND sender = $2 AND channel = $3`,
		tenantID, sender, channel).
		Scan(&s.TenantID, &s.Sender, &s.Channel, &s.State, &optedIn, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No row is not an error: a visitor who has never messaged us has no
		// session, and the caller starts one.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if optedIn.Valid {
		t := utc(optedIn.Time)
		s.OptedInAt = &t
	}
	s.CreatedAt, s.UpdatedAt = utc(s.CreatedAt), utc(s.UpdatedAt)
	return &s, nil
}

func (d *DB) SaveBoothSession(ctx context.Context, s *store.BoothSession) error {
	if s == nil {
		return errors.New("postgres: nil booth session")
	}
	var optedIn any
	if s.OptedInAt != nil {
		optedIn = *s.OptedInAt
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO booth_sessions (tenant_id, sender, channel, state, opted_in_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (tenant_id, sender, channel) DO UPDATE
		   SET state = EXCLUDED.state,
		       -- Never clear a recorded opt-in by saving a later state that
		       -- happens to carry none. Consent is a fact with a timestamp, not
		       -- a field that follows the menu position.
		       opted_in_at = COALESCE(booth_sessions.opted_in_at, EXCLUDED.opted_in_at),
		       updated_at = now()`,
		s.TenantID, s.Sender, s.Channel, s.State, optedIn)
	return err
}

func (d *DB) CreateBoothRelay(ctx context.Context, r *store.BoothRelay) error {
	if r == nil {
		return errors.New("postgres: nil booth relay")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO booth_relays
		   (tenant_id, ref, sender, channel, bridge_id, mesh_dest, body, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.TenantID, r.Ref, r.Sender, r.Channel, r.BridgeID, r.MeshDest, r.Body, r.ExpiresAt)
	return err
}

func (d *DB) GetBoothRelayByRef(ctx context.Context, tenantID, ref string) (*store.BoothRelay, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, ref, sender, channel, bridge_id, mesh_dest, body,
		        created_at, expires_at, closed_at
		 FROM booth_relays WHERE tenant_id = $1 AND ref = $2`, tenantID, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanBoothRelays(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// OpenBoothRelaysFor returns the conversations still open on one mesh node.
//
// It is what makes a reply with no ref routable: exactly one open row and the
// reply belongs to that visitor. More than one and the caller must ask, because
// guessing here means answering a stranger in front of an audience.
func (d *DB) OpenBoothRelaysFor(ctx context.Context, tenantID, bridgeID, meshDest string) ([]store.BoothRelay, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, ref, sender, channel, bridge_id, mesh_dest, body,
		        created_at, expires_at, closed_at
		 FROM booth_relays
		 WHERE tenant_id = $1 AND bridge_id = $2 AND mesh_dest = $3
		   AND closed_at IS NULL AND expires_at > now()
		 ORDER BY created_at`, tenantID, bridgeID, meshDest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanBoothRelays(rows)
}

func (d *DB) CloseBoothRelay(ctx context.Context, tenantID, ref string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE booth_relays SET closed_at = now()
		 WHERE tenant_id = $1 AND ref = $2 AND closed_at IS NULL`, tenantID, ref)
	return err
}

// CountBoothRelaysBySender is the per-visitor quota, counted from the relays
// themselves rather than a separate counter -- one source of truth, and it
// cannot drift from what was actually sent.
func (d *DB) CountBoothRelaysBySender(ctx context.Context, tenantID, sender string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_relays
		 WHERE tenant_id = $1 AND sender = $2 AND created_at >= $3`,
		tenantID, sender, since).Scan(&n)
	return n, err
}

func (d *DB) CountBoothRelays(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_relays WHERE tenant_id = $1 AND created_at >= $2`,
		tenantID, since).Scan(&n)
	return n, err
}

func scanBoothRelays(rows *sql.Rows) ([]store.BoothRelay, error) {
	var out []store.BoothRelay
	for rows.Next() {
		var r store.BoothRelay
		var closed sql.NullTime
		if err := rows.Scan(&r.TenantID, &r.Ref, &r.Sender, &r.Channel, &r.BridgeID,
			&r.MeshDest, &r.Body, &r.CreatedAt, &r.ExpiresAt, &closed); err != nil {
			return nil, err
		}
		if closed.Valid {
			t := utc(closed.Time)
			r.ClosedAt = &t
		}
		r.CreatedAt, r.ExpiresAt = utc(r.CreatedAt), utc(r.ExpiresAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExpiredOpenBoothRelays finds conversations whose reply never arrived, so the
// visitor can be told instead of left with silence (MESHSAT-1175).
func (d *DB) ExpiredOpenBoothRelays(ctx context.Context, tenantID string, now time.Time) ([]store.BoothRelay, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, ref, sender, channel, bridge_id, mesh_dest, body,
		        created_at, expires_at, closed_at
		 FROM booth_relays
		 WHERE tenant_id = $1 AND closed_at IS NULL AND expires_at <= $2
		 ORDER BY created_at`, tenantID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanBoothRelays(rows)
}

func (d *DB) RecordBoothSend(ctx context.Context, tenantID, id, recipient, channel, kind string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO booth_sends (tenant_id, id, recipient, channel, kind)
		 VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		tenantID, id, recipient, channel, kind)
	return err
}

func (d *DB) CountBoothSendsTo(ctx context.Context, tenantID, recipient string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_sends
		 WHERE tenant_id = $1 AND recipient = $2 AND created_at >= $3`,
		tenantID, recipient, since).Scan(&n)
	return n, err
}

func (d *DB) CountBoothSends(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_sends WHERE tenant_id = $1 AND created_at >= $2`,
		tenantID, since).Scan(&n)
	return n, err
}
