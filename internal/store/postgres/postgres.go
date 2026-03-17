// Package postgres implements store.Store using PostgreSQL via pgx.
// Used in cluster and k8s modes. Pure Go, no CGO.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// DB implements store.Store with PostgreSQL.
type DB struct {
	pool *pgxpool.Pool
}

// New connects to PostgreSQL and returns a DB.
func New(ctx context.Context, databaseURL string) (*DB, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() error                   { d.pool.Close(); return nil }
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Migrate creates tables if they don't exist.
func (d *DB) Migrate(ctx context.Context) error {
	for i, m := range migrations {
		if _, err := d.pool.Exec(ctx, m); err != nil {
			return fmt.Errorf("postgres migration %d: %w", i+1, err)
		}
	}
	return nil
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS devices (
		imei TEXT PRIMARY KEY,
		label TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'rockblock',
		notes TEXT NOT NULL DEFAULT '',
		last_seen TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL REFERENCES devices(imei) ON DELETE CASCADE,
		direction TEXT NOT NULL,
		channel TEXT NOT NULL DEFAULT 'iridium',
		momsn INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL DEFAULT '',
		raw_hex TEXT NOT NULL DEFAULT '',
		compressed BOOLEAN NOT NULL DEFAULT FALSE,
		status TEXT NOT NULL DEFAULT 'received',
		error TEXT NOT NULL DEFAULT '',
		lat DOUBLE PRECISION NOT NULL DEFAULT 0,
		lon DOUBLE PRECISION NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS webhook_configs (
		id TEXT PRIMARY KEY,
		url TEXT NOT NULL,
		secret TEXT NOT NULL DEFAULT '',
		events JSONB NOT NULL DEFAULT '[]',
		max_retries INTEGER NOT NULL DEFAULT 3,
		timeout_sec INTEGER NOT NULL DEFAULT 10,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS delivery_logs (
		id TEXT PRIMARY KEY,
		webhook_id TEXT NOT NULL,
		event TEXT NOT NULL,
		device_imei TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		attempt INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS positions (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL REFERENCES devices(imei) ON DELETE CASCADE,
		lat DOUBLE PRECISION NOT NULL,
		lon DOUBLE PRECISION NOT NULL,
		alt DOUBLE PRECISION NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT 'gps',
		cep DOUBLE PRECISION NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_positions_device ON positions(device_imei, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_device ON messages(device_imei, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		id TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
}

// --- Devices ---

func (d *DB) CreateDevice(ctx context.Context, dev *store.Device) error {
	_, err := d.pool.Exec(ctx,
		"INSERT INTO devices (imei, label, type, notes) VALUES ($1, $2, $3, $4)",
		dev.IMEI, dev.Label, dev.Type, dev.Notes)
	return err
}

func (d *DB) GetDevice(ctx context.Context, imei string) (*store.Device, error) {
	var dev store.Device
	err := d.pool.QueryRow(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE imei=$1", imei,
	).Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context) ([]store.Device, error) {
	rows, err := d.pool.Query(ctx, "SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices ORDER BY label, imei")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		if err := rows.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

func (d *DB) UpdateDevice(ctx context.Context, dev *store.Device) error {
	_, err := d.pool.Exec(ctx,
		"UPDATE devices SET label=$1, type=$2, notes=$3, updated_at=NOW() WHERE imei=$4",
		dev.Label, dev.Type, dev.Notes, dev.IMEI)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, imei string) error {
	_, err := d.pool.Exec(ctx, "DELETE FROM devices WHERE imei=$1", imei)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, imei string) error {
	_, err := d.pool.Exec(ctx, "UPDATE devices SET last_seen=NOW() WHERE imei=$1", imei)
	return err
}

// --- Messages ---

func (d *DB) InsertMessage(ctx context.Context, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		m.Compressed, m.Status, m.Error, m.Lat, m.Lon)
	return err
}

func (d *DB) ListMessages(ctx context.Context, deviceIMEI string, limit int) ([]store.Message, error) {
	query := "SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages"
	var args []interface{}
	argN := 1
	if deviceIMEI != "" {
		query += fmt.Sprintf(" WHERE device_imei=$%d", argN)
		args = append(args, deviceIMEI)
		argN++
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, limit)
	}
	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []store.Message
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN,
			&m.Text, &m.RawHex, &m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (d *DB) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	var m store.Message
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE id=$1", id,
	).Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN, &m.Text, &m.RawHex,
		&m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// --- Webhooks ---

func (d *DB) SaveWebhook(ctx context.Context, w *store.WebhookConfig) error {
	eventsJSON, _ := json.Marshal(w.Events)
	_, err := d.pool.Exec(ctx,
		`INSERT INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (id) DO UPDATE SET url=$2, secret=$3, events=$4, max_retries=$5, timeout_sec=$6, enabled=$7`,
		w.ID, w.URL, w.Secret, eventsJSON, w.MaxRetries, w.TimeoutSec, w.Enabled)
	return err
}

func (d *DB) ListWebhooks(ctx context.Context) ([]store.WebhookConfig, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var webhooks []store.WebhookConfig
	for rows.Next() {
		var w store.WebhookConfig
		var eventsJSON []byte
		if err := rows.Scan(&w.ID, &w.URL, &w.Secret, &eventsJSON, &w.MaxRetries, &w.TimeoutSec, &w.Enabled, &w.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(eventsJSON, &w.Events)
		webhooks = append(webhooks, w)
	}
	return webhooks, nil
}

func (d *DB) DeleteWebhook(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, "DELETE FROM webhook_configs WHERE id=$1", id)
	return err
}

// --- Delivery logs ---

func (d *DB) InsertDeliveryLog(ctx context.Context, l *store.DeliveryLog) error {
	if l.ID == "" {
		l.ID = fmt.Sprintf("dl-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO delivery_logs (id, webhook_id, event, device_imei, status_code, error, attempt)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		l.ID, l.WebhookID, l.Event, l.DeviceIMEI, l.StatusCode, l.Error, l.Attempt)
	return err
}

func (d *DB) ListDeliveryLogs(ctx context.Context, limit int) ([]store.DeliveryLog, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs ORDER BY created_at DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []store.DeliveryLog
	for rows.Next() {
		var l store.DeliveryLog
		if err := rows.Scan(&l.ID, &l.WebhookID, &l.Event, &l.DeviceIMEI, &l.StatusCode, &l.Error, &l.Attempt, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// --- Positions ---

func (d *DB) InsertPosition(ctx context.Context, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO positions (id, device_imei, lat, lon, alt, source, cep) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Source, p.CEP)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=$1 ORDER BY created_at DESC LIMIT 1",
		deviceIMEI,
	).Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Source, &p.CEP, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, deviceIMEI string, limit int) ([]store.Position, error) {
	rows, err := d.pool.Query(ctx,
		"SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=$1 ORDER BY created_at DESC LIMIT $2",
		deviceIMEI, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		if err := rows.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Source, &p.CEP, &p.CreatedAt); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}

// --- Audit log ---

func (d *DB) InsertAuditEntry(ctx context.Context, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip) VALUES ($1, $2, $3, $4, $5)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP)
	return err
}

func (d *DB) ListAuditEntries(ctx context.Context, limit int) ([]store.AuditEntry, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, action, actor, detail, ip, created_at FROM audit_log ORDER BY created_at DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, nil
}

// Compile-time check.
var _ store.Store = (*DB)(nil)
