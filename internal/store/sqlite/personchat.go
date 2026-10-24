package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The "@" lane (MESHSAT-1685); see the Postgres twin. Timestamps as
// time.DateTime strings, like the booth's, so max() and >= compare them in
// time order.

const personConversationCols = `tenant_id, id, device_id, device_kind, peer_kind, peer, tag, ref,
	last_out_at, last_in_at, last_out_message_id, last_in_message_id, created_at, updated_at`

type personRowScanner interface{ Scan(dest ...any) error }

func scanPersonConversation(s personRowScanner) (*store.PersonConversation, error) {
	var c store.PersonConversation
	var out, created, updated string
	var in sql.NullString
	if err := s.Scan(&c.TenantID, &c.ID, &c.DeviceID, &c.DeviceKind, &c.PeerKind, &c.Peer, &c.Tag, &c.Ref,
		&out, &in, &c.LastOutMessageID, &c.LastInMessageID, &created, &updated); err != nil {
		return nil, err
	}
	c.LastOutAt, c.CreatedAt, c.UpdatedAt = boothParse(out), boothParse(created), boothParse(updated)
	if in.Valid && in.String != "" {
		t := boothParse(in.String)
		c.LastInAt = &t
	}
	return &c, nil
}

func (d *DB) SavePersonConversationOut(ctx context.Context, c *store.PersonConversation) (*store.PersonConversation, error) {
	if c == nil {
		return nil, errors.New("sqlite: nil person conversation")
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO person_conversations
		   (tenant_id, id, device_id, device_kind, peer_kind, peer, tag, ref, last_out_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, id) DO UPDATE
		   SET last_out_at = max(person_conversations.last_out_at, excluded.last_out_at),
		       updated_at = datetime('now')`,
		c.TenantID, c.ID, c.DeviceID, c.DeviceKind, c.PeerKind, c.Peer, c.Tag, c.Ref, boothTime(c.LastOutAt)); err != nil {
		return nil, err
	}
	return scanPersonConversation(d.db.QueryRowContext(ctx,
		`SELECT `+personConversationCols+` FROM person_conversations WHERE tenant_id = ? AND id = ?`, c.TenantID, c.ID))
}

func (d *DB) GetPersonConversationByRef(ctx context.Context, tenantID, ref string) (*store.PersonConversation, error) {
	c, err := scanPersonConversation(d.db.QueryRowContext(ctx,
		`SELECT `+personConversationCols+` FROM person_conversations WHERE tenant_id = ? AND ref = ?`, tenantID, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (d *DB) PersonConversationsWith(ctx context.Context, tenantID, peerKind, peer string, since time.Time) ([]store.PersonConversation, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+personConversationCols+` FROM person_conversations
		 WHERE tenant_id = ? AND peer_kind = ? AND peer = ? AND last_out_at >= ?
		 ORDER BY last_out_at DESC`, tenantID, peerKind, peer, boothTime(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.PersonConversation
	for rows.Next() {
		c, err := scanPersonConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (d *DB) MarkPersonConversationIn(ctx context.Context, tenantID, id string, at time.Time, messageID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE person_conversations
		 SET last_in_at = ?, last_in_message_id = CASE WHEN ? = '' THEN last_in_message_id ELSE ? END, updated_at = datetime('now')
		 WHERE tenant_id = ? AND id = ?`, boothTime(at), messageID, messageID, tenantID, id)
	return err
}

func (d *DB) MarkPersonConversationOut(ctx context.Context, tenantID, id string, at time.Time, messageID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE person_conversations
		 SET last_out_at = max(last_out_at, ?),
		     last_out_message_id = CASE WHEN ? = '' THEN last_out_message_id ELSE ? END,
		     updated_at = datetime('now')
		 WHERE tenant_id = ? AND id = ?`, boothTime(at), messageID, messageID, tenantID, id)
	return err
}

func (d *DB) RecordPersonChatSend(ctx context.Context, s *store.PersonChatSend) error {
	if s == nil {
		return errors.New("sqlite: nil person chat send")
	}
	e2e := 0
	if s.E2E {
		e2e = 1
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO person_chat_sends (tenant_id, id, conversation_id, direction, e2e, bytes, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.TenantID, s.ID, s.ConversationID, s.Direction, e2e, s.Bytes, s.Status)
	return err
}

// TakePersonChatSlot: see the Postgres twin.
func (d *DB) TakePersonChatSlot(ctx context.Context, tenantID, key string, max int) (bool, error) {
	if max <= 0 {
		return false, nil
	}
	var n int
	err := d.db.QueryRowContext(ctx,
		`INSERT INTO person_chat_buckets (tenant_id, key, n, updated_at) VALUES (?, ?, 1, datetime('now'))
		 ON CONFLICT (tenant_id, key) DO UPDATE SET n = person_chat_buckets.n + 1, updated_at = datetime('now')
		   WHERE person_chat_buckets.n < ?
		 RETURNING n`, tenantID, key, max).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == 1 {
		_, _ = d.db.ExecContext(ctx,
			`DELETE FROM person_chat_buckets WHERE tenant_id = ? AND updated_at < datetime('now', '-3 days')`, tenantID)
	}
	return true, nil
}
