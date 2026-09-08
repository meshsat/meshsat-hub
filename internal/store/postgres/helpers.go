package postgres

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Conventions shared by every domain file of the Postgres store:
//   - placeholders are native $1..$n, written in the SQL text;
//   - BOOLEAN columns scan into bool directly (no boolToInt);
//   - TIMESTAMPTZ columns scan into time.Time (or sql.NullTime for NULL-able
//     columns) and are returned in UTC through utc/utcPtr;
//   - NOT NULL timestamp columns that the MariaDB schema filled with the
//     '1970-01-01' sentinel keep that sentinel (zeroTime) so callers' IsZero
//     checks keep working after the port;
//   - JSONB columns are written with jsonBytes and read with jsonInto;
//   - "not found" is sql.ErrNoRows, exactly as the sqlite and mariadb stores.

// zeroTime is the sentinel stored in NOT NULL timestamp columns that have no
// real value (last_seen, expires_at, acked_at ...).
var zeroTime = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// sentinelTime maps a zero time.Time to the stored sentinel.
func sentinelTime(t time.Time) time.Time {
	if t.IsZero() {
		return zeroTime
	}
	return t.UTC()
}

// fromSentinel maps the stored sentinel back to a zero time.Time.
func fromSentinel(t time.Time) time.Time {
	if !t.After(zeroTime) {
		return time.Time{}
	}
	return t.UTC()
}

// utc normalises a scanned timestamp to UTC (pgx returns the session zone).
func utc(t time.Time) time.Time { return t.UTC() }

// utcPtr converts a NULL-able scanned timestamp into *time.Time.
func utcPtr(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}

// nullTimePtr converts *time.Time into a driver value (NULL when nil).
func nullTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// jsonBytes marshals v for a JSONB column; nil slices become "[]"/"{}" by the
// caller's choice of zero value, never SQL NULL.
func jsonBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(b) == "null" {
		return []byte("[]"), nil
	}
	return b, nil
}

// jsonInto unmarshals a scanned JSONB value; empty input leaves v untouched.
func jsonInto(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
