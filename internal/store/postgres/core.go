package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Core domain: devices, messages, webhooks, delivery logs, positions.
// Semantics mirror internal/store/mariadb (same generated IDs, same
// ordering and limits, same tenant scoping, not-found = sql.ErrNoRows).

// --- Devices ---

const deviceColumns = "imei, label, type, notes, last_seen, created_at, updated_at"

func scanDevice(sc interface{ Scan(...any) error }, dev *store.Device) error {
	var lastSeen time.Time
	if err := sc.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &lastSeen, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
		return err
	}
	dev.LastSeen = fromSentinel(lastSeen)
	dev.CreatedAt = utc(dev.CreatedAt)
	dev.UpdatedAt = utc(dev.UpdatedAt)
	return nil
}

func (d *DB) CreateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO devices (imei, label, type, notes, tenant_id) VALUES ($1, $2, $3, $4, $5)",
		dev.IMEI, dev.Label, dev.Type, dev.Notes, tenantID)
	return err
}

func (d *DB) GetDevice(ctx context.Context, tenantID string, imei string) (*store.Device, error) {
	var dev store.Device
	row := d.db.QueryRowContext(ctx,
		"SELECT "+deviceColumns+" FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID)
	if err := scanDevice(row, &dev); err != nil {
		return nil, err
	}
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context, tenantID string) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+deviceColumns+" FROM devices WHERE tenant_id=$1 ORDER BY label, imei", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		if err := scanDevice(rows, &dev); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) UpdateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET label=$1, type=$2, notes=$3, updated_at=now() WHERE imei=$4 AND tenant_id=$5",
		dev.Label, dev.Type, dev.Notes, dev.IMEI, tenantID)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE devices SET last_seen=now() WHERE imei=$1 AND tenant_id=$2", imei, tenantID)
	return err
}

// --- Messages ---

const messageColumns = "id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at"

func scanMessage(sc interface{ Scan(...any) error }, m *store.Message) error {
	if err := sc.Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN,
		&m.Text, &m.RawHex, &m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt); err != nil {
		return err
	}
	m.CreatedAt = utc(m.CreatedAt)
	return nil
}

// InsertMessage stores a message; an existing row with the same ID is left
// untouched and store.ErrDuplicate is returned (ON CONFLICT DO NOTHING keeps
// the statement error-free so the dbwrap retry logic never sees a 23505).
func (d *DB) InsertMessage(ctx context.Context, tenantID string, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	var scheduledAt any
	if !m.ScheduledAt.IsZero() {
		scheduledAt = m.ScheduledAt.UTC()
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, tenant_id, scheduled_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (id) DO NOTHING`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		m.Compressed, m.Status, m.Error, m.Lat, m.Lon, tenantID, scheduledAt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (d *DB) ListMessages(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Message, error) {
	query := "SELECT " + messageColumns + " FROM messages WHERE tenant_id=$1"
	args := []any{tenantID}
	if deviceIMEI != "" {
		args = append(args, deviceIMEI)
		query += " AND device_imei=$" + strconv.Itoa(len(args))
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		args = append(args, limit)
		query += " LIMIT $" + strconv.Itoa(len(args))
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []store.Message
	for rows.Next() {
		var m store.Message
		if err := scanMessage(rows, &m); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (d *DB) GetMessage(ctx context.Context, tenantID string, id string) (*store.Message, error) {
	var m store.Message
	row := d.db.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE id=$1 AND tenant_id=$2", id, tenantID)
	if err := scanMessage(row, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (d *DB) ListScheduledMessages(ctx context.Context, before time.Time, limit int) ([]store.Message, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, scheduled_at, created_at, tenant_id
		 FROM messages WHERE scheduled_at IS NOT NULL AND scheduled_at <= $1 AND status = 'scheduled' ORDER BY scheduled_at ASC LIMIT $2`,
		before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []store.Message
	for rows.Next() {
		var m store.Message
		var scheduledAt sql.NullTime
		var tenantID string
		if err := rows.Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN,
			&m.Text, &m.RawHex, &m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon,
			&scheduledAt, &m.CreatedAt, &tenantID); err != nil {
			return nil, err
		}
		if scheduledAt.Valid {
			m.ScheduledAt = utc(scheduledAt.Time)
		}
		m.CreatedAt = utc(m.CreatedAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (d *DB) UpdateMessageStatus(ctx context.Context, _ string, id string, status string, errMsg string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE messages SET status=$1, error=$2 WHERE id=$3", status, errMsg, id)
	return err
}

// --- Webhooks ---

func (d *DB) SaveWebhook(ctx context.Context, tenantID string, w *store.WebhookConfig) error {
	eventsJSON, err := jsonBytes(w.Events)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE SET url=EXCLUDED.url, secret=EXCLUDED.secret, events=EXCLUDED.events,
		   max_retries=EXCLUDED.max_retries, timeout_sec=EXCLUDED.timeout_sec, enabled=EXCLUDED.enabled`,
		w.ID, w.URL, w.Secret, eventsJSON, w.MaxRetries, w.TimeoutSec, w.Enabled, tenantID)
	return err
}

func (d *DB) ListWebhooks(ctx context.Context, tenantID string) ([]store.WebhookConfig, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs WHERE tenant_id=$1", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var webhooks []store.WebhookConfig
	for rows.Next() {
		var w store.WebhookConfig
		var eventsJSON []byte
		if err := rows.Scan(&w.ID, &w.URL, &w.Secret, &eventsJSON, &w.MaxRetries, &w.TimeoutSec, &w.Enabled, &w.CreatedAt); err != nil {
			return nil, err
		}
		_ = jsonInto(eventsJSON, &w.Events)
		w.CreatedAt = utc(w.CreatedAt)
		webhooks = append(webhooks, w)
	}
	return webhooks, rows.Err()
}

func (d *DB) DeleteWebhook(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM webhook_configs WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// --- Delivery logs ---

func (d *DB) InsertDeliveryLog(ctx context.Context, tenantID string, l *store.DeliveryLog) error {
	if l.ID == "" {
		l.ID = fmt.Sprintf("dl-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO delivery_logs (id, webhook_id, event, device_imei, status_code, error, attempt, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		l.ID, l.WebhookID, l.Event, l.DeviceIMEI, l.StatusCode, l.Error, l.Attempt, tenantID)
	return err
}

func (d *DB) ListDeliveryLogs(ctx context.Context, tenantID string, limit int) ([]store.DeliveryLog, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2",
		tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var logs []store.DeliveryLog
	for rows.Next() {
		var l store.DeliveryLog
		if err := rows.Scan(&l.ID, &l.WebhookID, &l.Event, &l.DeviceIMEI, &l.StatusCode, &l.Error, &l.Attempt, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.CreatedAt = utc(l.CreatedAt)
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// --- Positions ---

const positionColumns = "id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at"

func scanPosition(sc interface{ Scan(...any) error }, p *store.Position) error {
	if err := sc.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &p.CreatedAt); err != nil {
		return err
	}
	p.CreatedAt = utc(p.CreatedAt)
	return nil
}

func (d *DB) InsertPosition(ctx context.Context, tenantID string, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO positions (id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)",
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Speed, p.Heading, p.Sats, p.Source, p.CEP, tenantID)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, tenantID string, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	row := d.db.QueryRowContext(ctx,
		"SELECT "+positionColumns+" FROM positions WHERE device_imei=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT 1",
		deviceIMEI, tenantID)
	if err := scanPosition(row, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Position, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+positionColumns+" FROM positions WHERE device_imei=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT $3",
		deviceIMEI, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		if err := scanPosition(rows, &p); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

// ListPositionsRange returns a page of positions in [from, to] (either bound
// may be zero = unbounded), newest first, plus the total count of the range.
func (d *DB) ListPositionsRange(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit, offset int) ([]store.Position, int, error) {
	var where strings.Builder
	where.WriteString(" WHERE device_imei=$1 AND tenant_id=$2")
	args := []any{deviceIMEI, tenantID}
	if !from.IsZero() {
		args = append(args, from.UTC())
		where.WriteString(" AND created_at >= $" + strconv.Itoa(len(args)))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		where.WriteString(" AND created_at <= $" + strconv.Itoa(len(args)))
	}

	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM positions"+where.String(), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + positionColumns + " FROM positions" + where.String() + " ORDER BY created_at DESC"
	fetchArgs := append([]any{}, args...)
	if limit > 0 {
		fetchArgs = append(fetchArgs, limit)
		query += " LIMIT $" + strconv.Itoa(len(fetchArgs))
	}
	if offset > 0 {
		fetchArgs = append(fetchArgs, offset)
		query += " OFFSET $" + strconv.Itoa(len(fetchArgs))
	}

	rows, err := d.db.QueryContext(ctx, query, fetchArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		if err := scanPosition(rows, &p); err != nil {
			return nil, 0, err
		}
		positions = append(positions, p)
	}
	return positions, total, rows.Err()
}
