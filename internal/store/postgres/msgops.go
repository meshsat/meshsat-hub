package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- The Hub app route hub_msg (MESHSAT-1584, HeMB-NG stage 2 C4) ---

const msgOpCols = `tenant_id, sender, op_id, mid, recipient, content_type, mid_expires_at, expires_at, content_sha256, body,
	state, conflict, rev, attempts, next_attempt_at, leased_until, recipient_answer, created_at, updated_at`

func scanMsgOp(sc interface{ Scan(...any) error }) (store.MsgOp, error) {
	var o store.MsgOp
	err := sc.Scan(&o.TenantID, &o.Sender, &o.OpID, &o.Mid, &o.Recipient, &o.ContentType, &o.MidExpiresAt, &o.ExpiresAt,
		&o.ContentSHA256, &o.Body, &o.State, &o.Conflict, &o.Rev, &o.Attempts, &o.NextAttemptAt, &o.LeasedUntil,
		&o.RecipientAnswer, &o.CreatedAt, &o.UpdatedAt)
	o.NextAttemptAt, o.LeasedUntil = utc(o.NextAttemptAt), utc(o.LeasedUntil)
	o.CreatedAt, o.UpdatedAt = utc(o.CreatedAt), utc(o.UpdatedAt)
	return o, err
}

func (d *DB) AdmitMsgOp(ctx context.Context, op *store.MsgOp) (store.MsgOpAdmission, error) {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return store.MsgOpAdmission{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// The key first: a repeat is answered from the stored operation whatever its mid.
	existing, err := scanMsgOp(tx.QueryRowContext(ctx, "SELECT "+msgOpCols+
		" FROM msg_operations WHERE tenant_id=$1 AND sender=$2 AND op_id=$3", op.TenantID, op.Sender, op.OpID))
	switch {
	case err == nil:
		return store.MsgOpAdmission{Outcome: store.MsgOpExisting, Existing: &existing}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return store.MsgOpAdmission{}, err
	}
	// The sender's mid index: one digest per mid. A concurrent insert of the same mid
	// waits for the other transaction, then the read sees what it committed.
	if _, err := tx.ExecContext(ctx, `INSERT INTO msg_mids (tenant_id, sender, mid, content_sha256) VALUES ($1,$2,$3,$4)
		ON CONFLICT DO NOTHING`, op.TenantID, op.Sender, op.Mid, op.ContentSHA256); err != nil {
		return store.MsgOpAdmission{}, err
	}
	var digest string
	if err := tx.QueryRowContext(ctx, `SELECT content_sha256 FROM msg_mids WHERE tenant_id=$1 AND sender=$2 AND mid=$3`,
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
	res, err := tx.ExecContext(ctx, `INSERT INTO msg_operations (`+msgOpCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT DO NOTHING`,
		op.TenantID, op.Sender, op.OpID, op.Mid, op.Recipient, op.ContentType, op.MidExpiresAt, op.ExpiresAt,
		op.ContentSHA256, op.Body, op.State, op.Conflict, op.Rev, op.Attempts, op.NextAttemptAt.UTC(),
		time.Unix(0, 0).UTC(), op.RecipientAnswer, now, now)
	if err != nil {
		return store.MsgOpAdmission{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another replica stored this key between the read and the insert.
		_ = tx.Rollback()
		got, err := d.GetMsgOp(ctx, op.TenantID, op.Sender, op.OpID)
		if err != nil {
			return store.MsgOpAdmission{}, err
		}
		return store.MsgOpAdmission{Outcome: store.MsgOpExisting, Existing: got}, nil
	}
	if err := tx.Commit(); err != nil {
		return store.MsgOpAdmission{}, err
	}
	return store.MsgOpAdmission{Outcome: store.MsgOpAdmitted}, nil
}

func (d *DB) GetMsgOp(ctx context.Context, tenantID, sender, opID string) (*store.MsgOp, error) {
	o, err := scanMsgOp(d.db.QueryRowContext(ctx, "SELECT "+msgOpCols+
		" FROM msg_operations WHERE tenant_id=$1 AND sender=$2 AND op_id=$3", tenantID, sender, opID))
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
	var pub sql.NullTime
	err := sc.Scan(&e.TenantID, &e.Sender, &e.OpID, &e.EventID, &e.Rev, &e.Class, &e.Kind, &e.Reason, &e.Authority,
		&e.Scope, &e.Detail, &e.At, &pub)
	if pub.Valid {
		t := utc(pub.Time)
		e.PublishedAt = &t
	}
	e.At = utc(e.At)
	return e, err
}

func listMsgEvents(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tenantID, sender, opID string) ([]store.MsgOpEvent, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+msgEventCols+
		" FROM msg_op_events WHERE tenant_id=$1 AND sender=$2 AND op_id=$3 ORDER BY rev", tenantID, sender, opID)
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
	// The operation row is the lock: concurrent events of one operation take turns, so
	// each projection sees every event before it.
	var rev int
	err = tx.QueryRowContext(ctx, `SELECT rev FROM msg_operations WHERE tenant_id=$1 AND sender=$2 AND op_id=$3 FOR UPDATE`,
		ev.TenantID, ev.Sender, ev.OpID).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM msg_op_events WHERE event_id=$1`, ev.EventID).Scan(&one); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	ev.Rev = rev + 1
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO msg_op_events (`+msgEventCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULL)`,
		ev.TenantID, ev.Sender, ev.OpID, ev.EventID, ev.Rev, ev.Class, ev.Kind, ev.Reason, ev.Authority, ev.Scope,
		ev.Detail, ev.At.UTC()); err != nil {
		return false, err
	}
	events, err := listMsgEvents(ctx, tx, ev.TenantID, ev.Sender, ev.OpID)
	if err != nil {
		return false, err
	}
	state, conflict := project(events)
	if _, err := tx.ExecContext(ctx, `UPDATE msg_operations SET state=$1, conflict=$2, rev=$3, updated_at=$4
		WHERE tenant_id=$5 AND sender=$6 AND op_id=$7`, state, conflict, ev.Rev, time.Now().UTC(), ev.TenantID, ev.Sender, ev.OpID); err != nil {
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
	return d.listMsgOps(ctx, `recipient_answer = '' AND state NOT IN ('completed', 'failed', 'expired')
		AND next_attempt_at <= $1 AND leased_until < $1 AND expires_at > $2 ORDER BY next_attempt_at, created_at LIMIT $3`,
		now.UTC(), now.UnixMilli(), limit)
}

func (d *DB) ClaimMsgOp(ctx context.Context, tenantID, sender, opID string, now, until time.Time) (bool, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET leased_until=$1, updated_at=$2
		WHERE tenant_id=$3 AND sender=$4 AND op_id=$5 AND leased_until < $6`, until.UTC(), time.Now().UTC(), tenantID, sender, opID, now.UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DB) MarkMsgOpAttempt(ctx context.Context, tenantID, sender, opID string, next time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET attempts=attempts+1, next_attempt_at=$1, leased_until=$2, updated_at=$3
		WHERE tenant_id=$4 AND sender=$5 AND op_id=$6`, next.UTC(), time.Unix(0, 0).UTC(), time.Now().UTC(), tenantID, sender, opID)
	return err
}

func (d *DB) SetMsgOpRecipientAnswer(ctx context.Context, tenantID, sender, opID, answer string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE msg_operations SET recipient_answer=$1, updated_at=$2
		WHERE tenant_id=$3 AND sender=$4 AND op_id=$5`, answer, time.Now().UTC(), tenantID, sender, opID)
	return err
}

func (d *DB) ListExpiredMsgOps(ctx context.Context, now time.Time, limit int) ([]store.MsgOp, error) {
	if limit <= 0 {
		limit = 50
	}
	return d.listMsgOps(ctx, `state NOT IN ('completed', 'failed', 'expired') AND expires_at <= $1 ORDER BY expires_at LIMIT $2`,
		now.UnixMilli(), limit)
}

func (d *DB) ListUnpublishedMsgOpEvents(ctx context.Context, limit int) ([]store.MsgOpEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+msgEventCols+
		" FROM msg_op_events WHERE published_at IS NULL ORDER BY at, rev LIMIT $1", limit)
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
	_, err := d.db.ExecContext(ctx, `UPDATE msg_op_events SET published_at=$1 WHERE event_id = ANY($2)`, at.UTC(), eventIDs)
	return err
}

func (d *DB) ReplayMsgOpEvents(ctx context.Context, tenantID, sender, opID string) (int, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE msg_op_events SET published_at=NULL WHERE tenant_id=$1 AND sender=$2 AND op_id=$3`,
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM msg_op_events e USING msg_operations o
		WHERE e.tenant_id=o.tenant_id AND e.sender=o.sender AND e.op_id=o.op_id AND o.expires_at < $1`, ms); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM msg_operations WHERE expires_at < $1`, ms)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM msg_mids m WHERE m.created_at < $1 AND NOT EXISTS (
		SELECT 1 FROM msg_operations o WHERE o.tenant_id=m.tenant_id AND o.sender=m.sender AND o.mid=m.mid)`, cutoff.UTC()); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
