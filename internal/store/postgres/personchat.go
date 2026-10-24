package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The "@" lane (MESHSAT-1685), tenant-scoped throughout. See
// store.PersonConversation.

const personConversationCols = `tenant_id, id, device_id, device_kind, peer_kind, peer, tag, ref,
	last_out_at, last_in_at, last_out_message_id, last_in_message_id, created_at, updated_at`

func scanPersonConversation(s rowScanner) (*store.PersonConversation, error) {
	var c store.PersonConversation
	var in sql.NullTime
	if err := s.Scan(&c.TenantID, &c.ID, &c.DeviceID, &c.DeviceKind, &c.PeerKind, &c.Peer, &c.Tag, &c.Ref,
		&c.LastOutAt, &in, &c.LastOutMessageID, &c.LastInMessageID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.LastOutAt, c.CreatedAt, c.UpdatedAt = utc(c.LastOutAt), utc(c.CreatedAt), utc(c.UpdatedAt)
	c.LastInAt = utcPtr(in)
	return &c, nil
}

// SavePersonConversationOut records that the device wrote to the person. A
// new conversation is inserted as given; an existing one keeps its tag and
// ref (a reply in flight carries the old ones) and only moves last_out_at
// forward. The row as stored is returned.
func (d *DB) SavePersonConversationOut(ctx context.Context, c *store.PersonConversation) (*store.PersonConversation, error) {
	if c == nil {
		return nil, errors.New("postgres: nil person conversation")
	}
	row := d.db.QueryRowContext(ctx,
		`INSERT INTO person_conversations
		   (tenant_id, id, device_id, device_kind, peer_kind, peer, tag, ref, last_out_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (tenant_id, id) DO UPDATE
		   SET last_out_at = GREATEST(person_conversations.last_out_at, EXCLUDED.last_out_at),
		       updated_at = now()
		 RETURNING `+personConversationCols,
		c.TenantID, c.ID, c.DeviceID, c.DeviceKind, c.PeerKind, c.Peer, c.Tag, c.Ref, c.LastOutAt.UTC())
	return scanPersonConversation(row)
}

func (d *DB) GetPersonConversationByRef(ctx context.Context, tenantID, ref string) (*store.PersonConversation, error) {
	c, err := scanPersonConversation(d.db.QueryRowContext(ctx,
		`SELECT `+personConversationCols+` FROM person_conversations WHERE tenant_id = $1 AND ref = $2`,
		tenantID, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// PersonConversationsWith lists the person's conversations in the tenant
// that the device wrote in since the given time, newest first.
func (d *DB) PersonConversationsWith(ctx context.Context, tenantID, peerKind, peer string, since time.Time) ([]store.PersonConversation, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+personConversationCols+` FROM person_conversations
		 WHERE tenant_id = $1 AND peer_kind = $2 AND peer = $3 AND last_out_at >= $4
		 ORDER BY last_out_at DESC`, tenantID, peerKind, peer, since.UTC())
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
		 SET last_in_at = $3, last_in_message_id = CASE WHEN $4 = '' THEN last_in_message_id ELSE $4 END, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`, tenantID, id, at.UTC(), messageID)
	return err
}

// MarkPersonConversationOut records an accepted send: it moves the reply
// window forward (never back) and keeps the mail's Message-ID for threading.
func (d *DB) MarkPersonConversationOut(ctx context.Context, tenantID, id string, at time.Time, messageID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE person_conversations
		 SET last_out_at = GREATEST(last_out_at, $3),
		     last_out_message_id = CASE WHEN $4 = '' THEN last_out_message_id ELSE $4 END,
		     updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`, tenantID, id, at.UTC(), messageID)
	return err
}

// RecordPersonChatSend writes one ledger row; a second write of the same id
// is the same send and changes nothing.
func (d *DB) RecordPersonChatSend(ctx context.Context, s *store.PersonChatSend) error {
	if s == nil {
		return errors.New("postgres: nil person chat send")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO person_chat_sends (tenant_id, id, conversation_id, direction, e2e, bytes, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, id) DO NOTHING`,
		s.TenantID, s.ID, s.ConversationID, s.Direction, s.E2E, s.Bytes, s.Status)
	return err
}

// TakePersonChatSlot counts one use of a ceiling in one statement: the row
// is created at 1, or counted up while it is under max. No row back means the
// ceiling is reached. Now and then the tenant's buckets older than three
// days are dropped (the windows are an hour and a day).
func (d *DB) TakePersonChatSlot(ctx context.Context, tenantID, key string, max int) (bool, error) {
	if max <= 0 {
		return false, nil
	}
	var n int
	err := d.db.QueryRowContext(ctx,
		`INSERT INTO person_chat_buckets (tenant_id, key, n, updated_at) VALUES ($1, $2, 1, now())
		 ON CONFLICT (tenant_id, key) DO UPDATE SET n = person_chat_buckets.n + 1, updated_at = now()
		   WHERE person_chat_buckets.n < $3
		 RETURNING n`, tenantID, key, max).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == 1 {
		_, _ = d.db.ExecContext(ctx,
			`DELETE FROM person_chat_buckets WHERE tenant_id = $1 AND updated_at < now() - interval '3 days'`, tenantID)
	}
	return true, nil
}
