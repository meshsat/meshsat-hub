package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The TTC booth flow (MESHSAT-1175). See the Postgres twin for why the return
// leg's correlation is in the database and not in a map.
//
// Timestamps are written and read as time.DateTime strings, which is the format
// sqlite's own datetime('now') produces and what the rest of this package parses
// (see bridges.go). Handing the driver a time.Time instead writes a format that
// scanning back into sql.NullTime then refuses -- the conformance suite caught
// exactly that, which is the reason it runs both dialects against one contract.

// boothTime renders a timestamp the way sqlite's datetime('now') does, so
// stored values are comparable and parseable regardless of the driver.
func boothTime(t time.Time) string { return t.UTC().Format(time.DateTime) }

// boothParse reads one back. A malformed value yields the zero time rather than
// an error: these are display and expiry fields, and failing a whole lookup over
// an unparseable timestamp would lose a conversation.
func boothParse(s string) time.Time {
	t, _ := time.Parse(time.DateTime, s)
	return t.UTC()
}

func (d *DB) GetBoothSession(ctx context.Context, tenantID, sender, channel string) (*store.BoothSession, error) {
	var s store.BoothSession
	var optedIn, createdAt, updatedAt sql.NullString
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, sender, channel, state, opted_in_at, created_at, updated_at
		 FROM booth_sessions WHERE tenant_id = ? AND sender = ? AND channel = ?`,
		tenantID, sender, channel).
		Scan(&s.TenantID, &s.Sender, &s.Channel, &s.State, &optedIn, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if optedIn.Valid && optedIn.String != "" {
		t := boothParse(optedIn.String)
		s.OptedInAt = &t
	}
	s.CreatedAt, s.UpdatedAt = boothParse(createdAt.String), boothParse(updatedAt.String)
	return &s, nil
}

func (d *DB) SaveBoothSession(ctx context.Context, s *store.BoothSession) error {
	if s == nil {
		return errors.New("sqlite: nil booth session")
	}
	var optedIn any
	if s.OptedInAt != nil {
		optedIn = boothTime(*s.OptedInAt)
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO booth_sessions (tenant_id, sender, channel, state, opted_in_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT (tenant_id, sender, channel) DO UPDATE
		   SET state = excluded.state,
		       opted_in_at = COALESCE(booth_sessions.opted_in_at, excluded.opted_in_at),
		       updated_at = datetime('now')`,
		s.TenantID, s.Sender, s.Channel, s.State, optedIn)
	return err
}

func (d *DB) CreateBoothRelay(ctx context.Context, r *store.BoothRelay) error {
	if r == nil {
		return errors.New("sqlite: nil booth relay")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO booth_relays
		   (tenant_id, ref, sender, channel, bridge_id, mesh_dest, body, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TenantID, r.Ref, r.Sender, r.Channel, r.BridgeID, r.MeshDest, r.Body, boothTime(r.ExpiresAt))
	return err
}

func (d *DB) GetBoothRelayByRef(ctx context.Context, tenantID, ref string) (*store.BoothRelay, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, ref, sender, channel, bridge_id, mesh_dest, body,
		        created_at, expires_at, closed_at
		 FROM booth_relays WHERE tenant_id = ? AND ref = ?`, tenantID, ref)
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

func (d *DB) OpenBoothRelaysFor(ctx context.Context, tenantID, bridgeID, meshDest string) ([]store.BoothRelay, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, ref, sender, channel, bridge_id, mesh_dest, body,
		        created_at, expires_at, closed_at
		 FROM booth_relays
		 WHERE tenant_id = ? AND bridge_id = ? AND mesh_dest = ?
		   AND closed_at IS NULL AND expires_at > datetime('now')
		 ORDER BY created_at`, tenantID, bridgeID, meshDest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanBoothRelays(rows)
}

func (d *DB) CloseBoothRelay(ctx context.Context, tenantID, ref string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE booth_relays SET closed_at = datetime('now')
		 WHERE tenant_id = ? AND ref = ? AND closed_at IS NULL`, tenantID, ref)
	return err
}

func (d *DB) CountBoothRelaysBySender(ctx context.Context, tenantID, sender string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_relays
		 WHERE tenant_id = ? AND sender = ? AND created_at >= ?`,
		tenantID, sender, boothTime(since)).Scan(&n)
	return n, err
}

func (d *DB) CountBoothRelays(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM booth_relays WHERE tenant_id = ? AND created_at >= ?`,
		tenantID, boothTime(since)).Scan(&n)
	return n, err
}

func scanBoothRelays(rows *sql.Rows) ([]store.BoothRelay, error) {
	var out []store.BoothRelay
	for rows.Next() {
		var r store.BoothRelay
		var closed, createdAt, expiresAt sql.NullString
		if err := rows.Scan(&r.TenantID, &r.Ref, &r.Sender, &r.Channel, &r.BridgeID,
			&r.MeshDest, &r.Body, &createdAt, &expiresAt, &closed); err != nil {
			return nil, err
		}
		if closed.Valid && closed.String != "" {
			t := boothParse(closed.String)
			r.ClosedAt = &t
		}
		r.CreatedAt, r.ExpiresAt = boothParse(createdAt.String), boothParse(expiresAt.String)
		out = append(out, r)
	}
	return out, rows.Err()
}
