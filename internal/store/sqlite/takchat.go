package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// TAK direct messages (MESHSAT-1706); see the Postgres twin. Timestamps as
// time.DateTime strings, like the booth's, so max() and >= compare them in
// time order.

func (d *DB) UpsertTAKContact(ctx context.Context, c *store.TAKContact) error {
	if c == nil {
		return errors.New("sqlite: nil tak contact")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO tak_contacts (tenant_id, uid, callsign, bridge_id, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, uid) DO UPDATE
		   SET callsign = excluded.callsign, bridge_id = excluded.bridge_id,
		       last_seen_at = max(tak_contacts.last_seen_at, excluded.last_seen_at)`,
		c.TenantID, c.UID, c.Callsign, c.BridgeID, boothTime(c.LastSeen))
	return err
}

func (d *DB) GetTAKContact(ctx context.Context, tenantID, uid string) (*store.TAKContact, error) {
	var c store.TAKContact
	var seen string
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, uid, callsign, bridge_id, last_seen_at FROM tak_contacts
		 WHERE tenant_id = ? AND uid = ?`, tenantID, uid).
		Scan(&c.TenantID, &c.UID, &c.Callsign, &c.BridgeID, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.LastSeen = boothParse(seen)
	return &c, nil
}

func (d *DB) ListTAKContacts(ctx context.Context, tenantID string, since time.Time) ([]store.TAKContact, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, uid, callsign, bridge_id, last_seen_at FROM tak_contacts
		 WHERE tenant_id = ? AND last_seen_at >= ? ORDER BY last_seen_at DESC, uid`,
		tenantID, boothTime(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TAKContact
	for rows.Next() {
		var c store.TAKContact
		var seen string
		if err := rows.Scan(&c.TenantID, &c.UID, &c.Callsign, &c.BridgeID, &seen); err != nil {
			return nil, err
		}
		c.LastSeen = boothParse(seen)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) SaveTAKChatMessage(ctx context.Context, m *store.TAKChatMessage) (bool, error) {
	if m == nil {
		return false, errors.New("sqlite: nil tak chat message")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO tak_chat_messages
		   (tenant_id, id, crew_uid, operator_uid, user_id, direction, text, bridge_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, id) DO NOTHING`,
		m.TenantID, m.ID, m.CrewUID, m.OperatorUID, m.UserID, m.Direction, m.Text, m.BridgeID, m.Status, boothTime(created))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		_, _ = d.db.ExecContext(ctx,
			`DELETE FROM tak_chat_messages WHERE tenant_id = ? AND created_at < ?`,
			m.TenantID, boothTime(time.Now().Add(-30*24*time.Hour)))
	}
	return n > 0, nil
}

func (d *DB) SetTAKChatMessageStatus(ctx context.Context, tenantID, id, status string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tak_chat_messages SET status = ? WHERE tenant_id = ? AND id = ?`, status, tenantID, id)
	return err
}

func (d *DB) ListTAKChatMessages(ctx context.Context, tenantID, crewUID string, limit int) ([]store.TAKChatMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, id, crew_uid, operator_uid, user_id, direction, text, bridge_id, status, created_at
		 FROM (SELECT * FROM tak_chat_messages WHERE tenant_id = ? AND crew_uid = ?
		       ORDER BY created_at DESC, id DESC LIMIT ?)
		 ORDER BY created_at, id`, tenantID, crewUID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TAKChatMessage
	for rows.Next() {
		var m store.TAKChatMessage
		var created string
		if err := rows.Scan(&m.TenantID, &m.ID, &m.CrewUID, &m.OperatorUID, &m.UserID, &m.Direction,
			&m.Text, &m.BridgeID, &m.Status, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = boothParse(created)
		out = append(out, m)
	}
	return out, rows.Err()
}
