package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// TAK direct messages (MESHSAT-1706), tenant-scoped throughout. See
// store.TAKContact and store.TAKChatMessage.

// takChatKeep is how long a chat message is kept.
const takChatKeep = "30 days"

// UpsertTAKContact records that a bridge exported the crew: the callsign and
// owner as given, the last time seen never moved back.
func (d *DB) UpsertTAKContact(ctx context.Context, c *store.TAKContact) error {
	if c == nil {
		return errors.New("postgres: nil tak contact")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO tak_contacts (tenant_id, uid, callsign, bridge_id, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, uid) DO UPDATE
		   SET callsign = EXCLUDED.callsign, bridge_id = EXCLUDED.bridge_id,
		       last_seen_at = GREATEST(tak_contacts.last_seen_at, EXCLUDED.last_seen_at)`,
		c.TenantID, c.UID, c.Callsign, c.BridgeID, c.LastSeen.UTC())
	return err
}

func (d *DB) GetTAKContact(ctx context.Context, tenantID, uid string) (*store.TAKContact, error) {
	var c store.TAKContact
	err := d.db.QueryRowContext(ctx,
		`SELECT tenant_id, uid, callsign, bridge_id, last_seen_at FROM tak_contacts
		 WHERE tenant_id = $1 AND uid = $2`, tenantID, uid).
		Scan(&c.TenantID, &c.UID, &c.Callsign, &c.BridgeID, &c.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.LastSeen = utc(c.LastSeen)
	return &c, nil
}

// ListTAKContacts lists the tenant's crews seen since the given time, newest
// first.
func (d *DB) ListTAKContacts(ctx context.Context, tenantID string, since time.Time) ([]store.TAKContact, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, uid, callsign, bridge_id, last_seen_at FROM tak_contacts
		 WHERE tenant_id = $1 AND last_seen_at >= $2 ORDER BY last_seen_at DESC, uid`,
		tenantID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TAKContact
	for rows.Next() {
		var c store.TAKContact
		if err := rows.Scan(&c.TenantID, &c.UID, &c.Callsign, &c.BridgeID, &c.LastSeen); err != nil {
			return nil, err
		}
		c.LastSeen = utc(c.LastSeen)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveTAKChatMessage stores one message and answers whether it was new: a
// second write of the same id is the same message and changes nothing. The
// tenant's messages older than takChatKeep go with each new one.
func (d *DB) SaveTAKChatMessage(ctx context.Context, m *store.TAKChatMessage) (bool, error) {
	if m == nil {
		return false, errors.New("postgres: nil tak chat message")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO tak_chat_messages
		   (tenant_id, id, crew_uid, operator_uid, user_id, direction, text, bridge_id, status, status_detail, client_ref, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 ON CONFLICT (tenant_id, id) DO NOTHING`,
		m.TenantID, m.ID, m.CrewUID, m.OperatorUID, m.UserID, m.Direction, m.Text, m.BridgeID, m.Status,
		m.StatusDetail, m.ClientRef, created.UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		_, _ = d.db.ExecContext(ctx,
			`DELETE FROM tak_chat_messages WHERE tenant_id = $1 AND created_at < now() - interval '`+takChatKeep+`'`, m.TenantID)
	}
	return n > 0, nil
}

// SetTAKChatMessageStatus records the Hub's own word on a message after it
// was stored: a send the bus refused.
func (d *DB) SetTAKChatMessageStatus(ctx context.Context, tenantID, id, status, detail string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tak_chat_messages SET status = $3, status_detail = $4 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, status, detail)
	return err
}

// takChatFinal are the bridge's words a later "queued" may not undo: the
// ledger's row was already settled, whatever order the two reports arrived in.
const takChatFinal = "'delivered', 'failed', 'undelivered'"

// SetTAKChatMessageOutcome records a bridge's word on a message it was handed
// (MESHSAT-1746): see store.Store.
func (d *DB) SetTAKChatMessageOutcome(ctx context.Context, tenantID, bridgeID, id, status, detail string) (*store.TAKChatMessage, error) {
	var m store.TAKChatMessage
	err := d.db.QueryRowContext(ctx,
		`UPDATE tak_chat_messages SET status = $4, status_detail = $5
		 WHERE tenant_id = $1 AND id = $2 AND bridge_id = $3 AND direction = 'out'
		   AND NOT ($4 = 'queued' AND status IN (`+takChatFinal+`))
		 RETURNING tenant_id, id, crew_uid, operator_uid, user_id, direction, text, bridge_id, status, status_detail, client_ref, created_at`,
		tenantID, id, bridgeID, status, detail).
		Scan(&m.TenantID, &m.ID, &m.CrewUID, &m.OperatorUID, &m.UserID, &m.Direction,
			&m.Text, &m.BridgeID, &m.Status, &m.StatusDetail, &m.ClientRef, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = utc(m.CreatedAt)
	return &m, nil
}

// ListTAKChatMessages lists the last limit messages with one crew, oldest
// first.
func (d *DB) ListTAKChatMessages(ctx context.Context, tenantID, crewUID string, limit int) ([]store.TAKChatMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT tenant_id, id, crew_uid, operator_uid, user_id, direction, text, bridge_id, status, status_detail, client_ref, created_at
		 FROM (SELECT * FROM tak_chat_messages WHERE tenant_id = $1 AND crew_uid = $2
		       ORDER BY created_at DESC, id DESC LIMIT $3) recent
		 ORDER BY created_at, id`, tenantID, crewUID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TAKChatMessage
	for rows.Next() {
		var m store.TAKChatMessage
		if err := rows.Scan(&m.TenantID, &m.ID, &m.CrewUID, &m.OperatorUID, &m.UserID, &m.Direction,
			&m.Text, &m.BridgeID, &m.Status, &m.StatusDetail, &m.ClientRef, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.CreatedAt = utc(m.CreatedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}
