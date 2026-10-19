package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- The Hub app route hub_msg (MESHSAT-1584, HeMB-NG stage 2 C4) ---
//
// Each transaction starts with a write, so it holds SQLite's write lock from its first
// statement: a transaction that reads first and writes later can be refused at the upgrade
// when another writer came in between, which the busy timeout does not cover.

const msgOpCols = `tenant_id, sender, op_id, mid, recipient, content_type, mid_expires_at, expires_at, content_sha256, body,
	state, conflict, rev, attempts, next_attempt_at, leased_until, recipient_answer, created_at, updated_at`

var epoch = time.Unix(0, 0).UTC()

func scanMsgOp(sc interface{ Scan(...any) error }) (store.MsgOp, error) {
	var o store.MsgOp
	var conflict int
	var next, leased, created, updated string
	err := sc.Scan(&o.TenantID, &o.Sender, &o.OpID, &o.Mid, &o.Recipient, &o.ContentType, &o.MidExpiresAt, &o.ExpiresAt,
		&o.ContentSHA256, &o.Body, &o.State, &conflict, &o.Rev, &o.Attempts, &next, &leased, &o.RecipientAnswer,
		&created, &updated)
	o.Conflict = conflict != 0
	o.NextAttemptAt, o.LeasedUntil, o.CreatedAt, o.UpdatedAt = parseTime(next), parseTime(leased), parseTime(created), parseTime(updated)
	return o, err
}

func (d *DB) AdmitMsgOp(ctx context.Context, op *store.MsgOp) (store.MsgOpAdmission, error) {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return store.MsgOpAdmission{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// The sender's mid index first (the write lock), then the key: a repeat is answered from
	// the stored operation whatever its mid, and the rollback takes the index row back.
	if _, err := tx.ExecContext(ctx, `INSERT INTO msg_mids (tenant_id, sender, mid, content_sha256) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, op.TenantID, op.Sender, op.Mid, op.ContentSHA256); err != nil {
		return store.MsgOpAdmission{}, err
	}
	existing, err := scanMsgOp(tx.QueryRowContext(ctx, "SELECT "+msgOpCols+
		" FROM msg_operations WHERE tenant_id=? AND sender=? AND op_id=?", op.TenantID, op.Sender, op.OpID))
	switch {
	case err == nil:
		return store.MsgOpAdmission{Outcome: store.MsgOpExisting, Existing: &existing}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return store.MsgOpAdmission{}, err
	}
	var digest string
	if err := tx.QueryRowContext(ctx, `SELECT content_sha256 FROM msg_mids WHERE tenant_id=? AND sender=? AND mid=?`,
		op.TenantID, op.Sender, op.Mid).Scan(&digest); err != nil {
		return store.MsgOpAdmission{}, err
	}
	if digest != op.ContentSHA256 {
		return store.MsgOpAdmission{Outcome: store.MsgOpMidConflict}, nil
	}
	now := time.Now().UTC()
	if op.State == "" {
		op.State = "pending"
	}
	if op.NextAttemptAt.IsZero() {
		op.NextAttemptAt = now
	}
	op.CreatedAt, op.UpdatedAt = now, now
	if _, err := tx.ExecContext(ctx, `INSERT INTO msg_operations (`+msgOpCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.TenantID, op.Sender, op.OpID, op.Mid, op.Recipient, op.ContentType, op.MidExpiresAt, op.ExpiresAt,
		op.ContentSHA256, op.Body, op.State, boolInt(op.Conflict), op.Rev, op.Attempts, fmtTime(op.NextAttemptAt.UTC()),
		fmtTime(epoch), op.RecipientAnswer, fmtTime(now), fmtTime(now)); err != nil {
		return store.MsgOpAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.MsgOpAdmission{}, err
	}
	return store.MsgOpAdmission{Outcome: store.MsgOpAdmitted}, nil
}

func (d *DB) GetMsgOp(ctx context.Context, tenantID, sender, opID string) (*store.MsgOp, error) {
	o, err := scanMsgOp(d.db.QueryRowContext(ctx, "SELECT "+msgOpCols+
		" FROM msg_operations WHERE tenant_id=? AND sender=? AND op_id=?", tenantID, sender, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

const msgEventCols = `tenant_id, sender, op_id, event_id, rev, class, kind, reason, authority, scope, detail, at, published_at`

func scanMsgEvent(sc interface{ Scan(...any) error }) (store.MsgOpEvent, error) {
	var e store.MsgOpEvent
	var at string
	var pub sql.NullString
	err := sc.Scan(&e.TenantID, &e.Sender, &e.OpID, &e.EventID, &e.Rev, &e.Class, &e.Kind, &e.Reason, &e.Authority,
		&e.Scope, &e.Detail, &at, &pub)
	e.At = parseTime(at)
	if pub.Valid && pub.String != "" {
		t := parseTime(pub.String)
		e.PublishedAt = &t
	}
	return e, err
}

func listMsgEvents(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tenantID, sender, opID string) ([]store.MsgOpEvent, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+msgEventCols+
		" FROM msg_op_events WHERE tenant_id=? AND sender=? AND op_id=? ORDER BY rev", tenantID, sender, opID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.MsgOpEvent{}
	for rows.Next() {
		e, err := scanMsgEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) AddMsgOpEvent(ctx context.Context, ev *store.MsgOpEvent, project func([]store.MsgOpEvent) (string, bool)) (bool, error) {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	// The write lock, and the operation's existence, in one statement.
	res, err := tx.ExecContext(ctx, `UPDATE msg_operations SET updated_at=? WHERE tenant_id=? AND sender=? AND op_id=?`,
		fmtTime(now), ev.TenantID, ev.Sender, ev.OpID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, store.ErrNotFound
	}
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM msg_op_events WHERE event_id=?`, ev.EventID).Scan(&one); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	var rev int
	if err := tx.QueryRowContext(ctx, `SELECT rev FROM msg_operations WHERE tenant_id=? AND sender=? AND op_id=?`,
		ev.TenantID, ev.Sender, ev.OpID).Scan(&rev); err != nil {
		return false, err
	}
	ev.Rev = rev + 1
	if ev.At.IsZero() {
		ev.At = now
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO msg_op_events (`+msgEventCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		ev.TenantID, ev.Sender, ev.OpID, ev.EventID, ev.Rev, ev.Class, ev.Kind, ev.Reason, ev.Authority, ev.Scope,
		ev.Detail, fmtTime(ev.At.UTC())); err != nil {
		return false, err
	}
	events, err := listMsgEvents(ctx, tx, ev.TenantID, ev.Sender, ev.OpID)
	if err != nil {
		return false, err
	}
	state, conflict := project(events)
	if _, err := tx.ExecContext(ctx, `UPDATE msg_operations SET state=?, conflict=?, rev=?, updated_at=?
		WHERE tenant_id=? AND sender=? AND op_id=?`, state, boolInt(conflict), ev.Rev, fmtTime(now), ev.TenantID, ev.Sender, ev.OpID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DB) ListMsgOpEvents(ctx context.Context, tenantID, sender, opID string) ([]store.MsgOpEvent, error) {
	return listMsgEvents(ctx, d.db, tenantID, sender, opID)
}

func (d *DB) listMsgOps(ctx context.Context, where string, args ...any) ([]store.MsgOp, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+msgOpCols+" FROM msg_operations WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.MsgOp{}
	for rows.Next() {
		o, err := scanMsgOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) ListDueMsgOps(ctx context.Context, now time.Time, limit int) ([]store.MsgOp, error) {
	if limit <= 0 {
		limit = 50
	}
	n := fmtTime(now.UTC())
	return d.listMsgOps(ctx, `recipient_answer = '' AND state NOT IN ('completed', 'failed', 'expired')
		AND next_attempt_at <= ? AND leased_until < ? AND expires_at > ? ORDER BY next_attempt_at, created_at LIMIT ?`,
		n, n, now.UnixMilli(), limit)
}

func (d *DB) ClaimMsgOp(ctx context.Context, tenantID, sender, opID string, now, until time.Time) (bool, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET leased_until=?, updated_at=?
		WHERE tenant_id=? AND sender=? AND op_id=? AND leased_until < ?`, fmtTime(until.UTC()), fmtTime(time.Now().UTC()), tenantID, sender, opID, fmtTime(now.UTC()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) MarkMsgOpAttempt(ctx context.Context, tenantID, sender, opID string, next time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET attempts=attempts+1, next_attempt_at=?, leased_until=?, updated_at=?
		WHERE tenant_id=? AND sender=? AND op_id=?`, fmtTime(next.UTC()), fmtTime(epoch), fmtTime(time.Now().UTC()), tenantID, sender, opID)
	return err
}

func (d *DB) SetMsgOpRecipientAnswer(ctx context.Context, tenantID, sender, opID, answer string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET recipient_answer=?, updated_at=?
		WHERE tenant_id=? AND sender=? AND op_id=?`, answer, fmtTime(time.Now().UTC()), tenantID, sender, opID)
	return err
}

func (d *DB) ListExpiredMsgOps(ctx context.Context, now time.Time, limit int) ([]store.MsgOp, error) {
	if limit <= 0 {
		limit = 50
	}
	return d.listMsgOps(ctx, `state NOT IN ('completed', 'failed', 'expired') AND expires_at <= ? ORDER BY expires_at LIMIT ?`,
		now.UnixMilli(), limit)
}

func (d *DB) ListUnpublishedMsgOpEvents(ctx context.Context, limit int) ([]store.MsgOpEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+msgEventCols+
		" FROM msg_op_events WHERE published_at IS NULL ORDER BY at, rev LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []store.MsgOpEvent{}
	for rows.Next() {
		e, err := scanMsgEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) MarkMsgOpEventsPublished(ctx context.Context, eventIDs []string, at time.Time) error {
	if len(eventIDs) == 0 {
		return nil
	}
	args := []any{fmtTime(at.UTC())}
	for _, id := range eventIDs {
		args = append(args, id)
	}
	_, err := d.db.ExecContext(ctx, `UPDATE msg_op_events SET published_at=? WHERE event_id IN (?`+
		strings.Repeat(", ?", len(eventIDs)-1)+`)`, args...)
	return err
}

func (d *DB) ReplayMsgOpEvents(ctx context.Context, tenantID, sender, opID string) (int, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE msg_op_events SET published_at=NULL WHERE tenant_id=? AND sender=? AND op_id=?`,
		tenantID, sender, opID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (d *DB) PruneMsgOps(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	ms := cutoff.UnixMilli()
	if _, err := tx.ExecContext(ctx, `DELETE FROM msg_op_events WHERE EXISTS (SELECT 1 FROM msg_operations o
		WHERE o.tenant_id=msg_op_events.tenant_id AND o.sender=msg_op_events.sender AND o.op_id=msg_op_events.op_id AND o.expires_at < ?)`, ms); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM msg_operations WHERE expires_at < ?`, ms)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM msg_mids WHERE created_at < ? AND NOT EXISTS (
		SELECT 1 FROM msg_operations o WHERE o.tenant_id=msg_mids.tenant_id AND o.sender=msg_mids.sender AND o.mid=msg_mids.mid)`,
		fmtTime(cutoff.UTC())); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
