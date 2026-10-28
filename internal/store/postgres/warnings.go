package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The warning relay (MESHSAT-1734). warnings is platform-wide (a public
// warning names an area, not a tenant); warning_matches is tenant-scoped.

// warningKeep is how long a warning row outlives its last sighting.
const warningKeep = "30 days"

// UpsertWarning records a feed's message. It answers changed = true when the
// row is new or its sent time or document changed (a re-sent identifier),
// false for a message seen before, whose last_seen is moved. The feed table
// is pruned of rows unseen for 30 days on every call.
func (d *DB) UpsertWarning(ctx context.Context, w *store.Warning) (bool, error) {
	if w == nil {
		return false, errors.New("postgres: nil warning")
	}
	now := time.Now().UTC()
	var sent time.Time
	var hash string
	err := d.db.QueryRowContext(ctx,
		`SELECT sent, raw_hash FROM warnings WHERE source = $1 AND identifier = $2`, w.Source, w.Identifier).Scan(&sent, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case sent.Equal(w.Sent) && hash == w.RawHash:
		_, err = d.db.ExecContext(ctx, `UPDATE warnings SET last_seen = $3 WHERE source = $1 AND identifier = $2`, w.Source, w.Identifier, now)
		return false, err
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO warnings (source, identifier, sender, sender_name, msg_type, status, refs, sent, effective, expires,
		   severity, urgency, certainty, category, event, headline, language, areas, text, raw_hash, first_seen, last_seen)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $21)
		 ON CONFLICT (source, identifier) DO UPDATE
		   SET sender = EXCLUDED.sender, sender_name = EXCLUDED.sender_name, msg_type = EXCLUDED.msg_type,
		       status = EXCLUDED.status, refs = EXCLUDED.refs, sent = EXCLUDED.sent, effective = EXCLUDED.effective,
		       expires = EXCLUDED.expires, severity = EXCLUDED.severity, urgency = EXCLUDED.urgency,
		       certainty = EXCLUDED.certainty, category = EXCLUDED.category, event = EXCLUDED.event,
		       headline = EXCLUDED.headline, language = EXCLUDED.language, areas = EXCLUDED.areas,
		       text = EXCLUDED.text, raw_hash = EXCLUDED.raw_hash, last_seen = EXCLUDED.last_seen`,
		w.Source, w.Identifier, w.Sender, w.SenderName, w.MsgType, w.Status, w.References, w.Sent.UTC(),
		nullTime(&w.Effective), nullTime(&w.Expires), w.Severity, w.Urgency, w.Certainty, w.Category, w.Event,
		w.Headline, w.Language, orJSONArray(w.Areas), w.Text, w.RawHash, now); err != nil {
		return false, err
	}
	_, _ = d.db.ExecContext(ctx, `DELETE FROM warnings WHERE last_seen < now() - interval '`+warningKeep+`'`)
	return true, nil
}

// GetWarning answers one feed row, nil when there is none.
func (d *DB) GetWarning(ctx context.Context, source, identifier string) (*store.Warning, error) {
	var w store.Warning
	var effective, expires sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT source, identifier, sender, sender_name, msg_type, status, refs, sent, effective, expires,
		   severity, urgency, certainty, category, event, headline, language, areas, text, raw_hash, first_seen, last_seen
		 FROM warnings WHERE source = $1 AND identifier = $2`, source, identifier).
		Scan(&w.Source, &w.Identifier, &w.Sender, &w.SenderName, &w.MsgType, &w.Status, &w.References, &w.Sent,
			&effective, &expires, &w.Severity, &w.Urgency, &w.Certainty, &w.Category, &w.Event, &w.Headline,
			&w.Language, &w.Areas, &w.Text, &w.RawHash, &w.FirstSeen, &w.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.Effective, w.Expires = effective.Time, expires.Time
	return &w, nil
}

// UpsertWarningMatch records that a warning matched the tenant. A second
// match of the same message (a re-poll, the other replica) keeps the first
// matched_at and takes the latest bridges and detail.
func (d *DB) UpsertWarningMatch(ctx context.Context, m *store.WarningMatch) error {
	if m == nil {
		return errors.New("postgres: nil warning match")
	}
	at := m.MatchedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO warning_matches (tenant_id, source, identifier, msg_type, test, bridges, detail, matched_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (tenant_id, source, identifier) DO UPDATE
		   SET msg_type = EXCLUDED.msg_type, test = EXCLUDED.test, bridges = EXCLUDED.bridges, detail = EXCLUDED.detail`,
		m.TenantID, m.Source, m.Identifier, m.MsgType, m.Test, orJSONArray(m.Bridges), m.Detail, at.UTC())
	return err
}

// ListTenantWarnings lists the tenant's matched warnings, newest first.
func (d *DB) ListTenantWarnings(ctx context.Context, tenantID string, limit int) ([]store.TenantWarning, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT w.source, w.identifier, w.sender, w.sender_name, w.msg_type, w.status, w.refs, w.sent, w.effective, w.expires,
		   w.severity, w.urgency, w.certainty, w.category, w.event, w.headline, w.language, w.areas, w.text, w.raw_hash,
		   w.first_seen, w.last_seen, m.test, m.bridges, m.detail, m.matched_at
		 FROM warning_matches m JOIN warnings w ON w.source = m.source AND w.identifier = m.identifier
		 WHERE m.tenant_id = $1 ORDER BY m.matched_at DESC, w.sent DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.TenantWarning
	for rows.Next() {
		var w store.TenantWarning
		var effective, expires sql.NullTime
		if err := rows.Scan(&w.Source, &w.Identifier, &w.Sender, &w.SenderName, &w.MsgType, &w.Status, &w.References, &w.Sent,
			&effective, &expires, &w.Severity, &w.Urgency, &w.Certainty, &w.Category, &w.Event, &w.Headline,
			&w.Language, &w.Areas, &w.Text, &w.RawHash, &w.FirstSeen, &w.LastSeen, &w.Test, &w.Bridges, &w.Detail, &w.MatchedAt); err != nil {
			return nil, err
		}
		w.Effective, w.Expires = effective.Time, expires.Time
		out = append(out, w)
	}
	return out, rows.Err()
}

func orJSONArray(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}
