package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The warning relay (MESHSAT-1734); see the Postgres twin. Times are
// time.DateTime strings, like the booth's, so < compares them in time order;
// an unset time is "".

func (d *DB) UpsertWarning(ctx context.Context, w *store.Warning) (bool, error) {
	if w == nil {
		return false, errors.New("sqlite: nil warning")
	}
	now := time.Now()
	var sent, hash string
	err := d.db.QueryRowContext(ctx,
		`SELECT sent, raw_hash FROM warnings WHERE source = ? AND identifier = ?`, w.Source, w.Identifier).Scan(&sent, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case sent == boothTime(w.Sent) && hash == w.RawHash:
		_, err = d.db.ExecContext(ctx, `UPDATE warnings SET last_seen = ? WHERE source = ? AND identifier = ?`, boothTime(now), w.Source, w.Identifier)
		return false, err
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO warnings (source, identifier, sender, sender_name, msg_type, status, refs, sent, effective, expires,
		   severity, urgency, certainty, category, event, headline, language, areas, text, raw_hash, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (source, identifier) DO UPDATE
		   SET sender = excluded.sender, sender_name = excluded.sender_name, msg_type = excluded.msg_type,
		       status = excluded.status, refs = excluded.refs, sent = excluded.sent, effective = excluded.effective,
		       expires = excluded.expires, severity = excluded.severity, urgency = excluded.urgency,
		       certainty = excluded.certainty, category = excluded.category, event = excluded.event,
		       headline = excluded.headline, language = excluded.language, areas = excluded.areas,
		       text = excluded.text, raw_hash = excluded.raw_hash, last_seen = excluded.last_seen`,
		w.Source, w.Identifier, w.Sender, w.SenderName, w.MsgType, w.Status, w.References, boothTime(w.Sent),
		optTime(w.Effective), optTime(w.Expires), w.Severity, w.Urgency, w.Certainty, w.Category, w.Event,
		w.Headline, w.Language, orJSONArray(w.Areas), w.Text, w.RawHash, boothTime(now), boothTime(now)); err != nil {
		return false, err
	}
	_, _ = d.db.ExecContext(ctx, `DELETE FROM warnings WHERE last_seen < ?`, boothTime(now.Add(-30*24*time.Hour)))
	return true, nil
}

func (d *DB) GetWarning(ctx context.Context, source, identifier string) (*store.Warning, error) {
	var w store.Warning
	var sent, effective, expires, first, last string
	err := d.db.QueryRowContext(ctx,
		`SELECT source, identifier, sender, sender_name, msg_type, status, refs, sent, effective, expires,
		   severity, urgency, certainty, category, event, headline, language, areas, text, raw_hash, first_seen, last_seen
		 FROM warnings WHERE source = ? AND identifier = ?`, source, identifier).
		Scan(&w.Source, &w.Identifier, &w.Sender, &w.SenderName, &w.MsgType, &w.Status, &w.References, &sent,
			&effective, &expires, &w.Severity, &w.Urgency, &w.Certainty, &w.Category, &w.Event, &w.Headline,
			&w.Language, &w.Areas, &w.Text, &w.RawHash, &first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.Sent, w.Effective, w.Expires = boothParse(sent), parseOptTime(effective), parseOptTime(expires)
	w.FirstSeen, w.LastSeen = boothParse(first), boothParse(last)
	return &w, nil
}

func (d *DB) UpsertWarningMatch(ctx context.Context, m *store.WarningMatch) error {
	if m == nil {
		return errors.New("sqlite: nil warning match")
	}
	at := m.MatchedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO warning_matches (tenant_id, source, identifier, msg_type, test, bridges, detail, matched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, source, identifier) DO UPDATE
		   SET msg_type = excluded.msg_type, test = excluded.test, bridges = excluded.bridges, detail = excluded.detail`,
		m.TenantID, m.Source, m.Identifier, m.MsgType, boolInt(m.Test), orJSONArray(m.Bridges), m.Detail, boothTime(at))
	return err
}

func (d *DB) ListTenantWarnings(ctx context.Context, tenantID string, limit int) ([]store.TenantWarning, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT w.source, w.identifier, w.sender, w.sender_name, w.msg_type, w.status, w.refs, w.sent, w.effective, w.expires,
		   w.severity, w.urgency, w.certainty, w.category, w.event, w.headline, w.language, w.areas, w.text, w.raw_hash,
		   w.first_seen, w.last_seen, m.test, m.bridges, m.detail, m.matched_at
		 FROM warning_matches m JOIN warnings w ON w.source = m.source AND w.identifier = m.identifier
		 WHERE m.tenant_id = ? ORDER BY m.matched_at DESC, w.sent DESC LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TenantWarning
	for rows.Next() {
		var w store.TenantWarning
		var sent, effective, expires, first, last, matched string
		var test int
		if err := rows.Scan(&w.Source, &w.Identifier, &w.Sender, &w.SenderName, &w.MsgType, &w.Status, &w.References, &sent,
			&effective, &expires, &w.Severity, &w.Urgency, &w.Certainty, &w.Category, &w.Event, &w.Headline,
			&w.Language, &w.Areas, &w.Text, &w.RawHash, &first, &last, &test, &w.Bridges, &w.Detail, &matched); err != nil {
			return nil, err
		}
		w.Sent, w.Effective, w.Expires = boothParse(sent), parseOptTime(effective), parseOptTime(expires)
		w.FirstSeen, w.LastSeen, w.MatchedAt = boothParse(first), boothParse(last), boothParse(matched)
		w.Test = test != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

func optTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return boothTime(t)
}

func parseOptTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	return boothParse(s)
}

func orJSONArray(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}
