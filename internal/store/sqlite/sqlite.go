// Package sqlite implements store.Store using modernc.org/sqlite (pure Go, no CGO).
// Used in standalone mode (single Docker Compose).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// DB implements store.Store with SQLite.
type DB struct {
	db *sql.DB
}

// New opens a SQLite database at the given path.
func New(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON&_synchronous=NORMAL", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite write serialization
	return &DB{db: conn}, nil
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Migrate runs all schema migrations.
func (d *DB) Migrate(ctx context.Context) error {
	for i, m := range migrations {
		if _, err := d.db.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
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
		last_seen TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL REFERENCES devices(imei) ON DELETE CASCADE,
		direction TEXT NOT NULL,
		channel TEXT NOT NULL DEFAULT 'iridium',
		momsn INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL DEFAULT '',
		raw_hex TEXT NOT NULL DEFAULT '',
		compressed INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'received',
		error TEXT NOT NULL DEFAULT '',
		lat REAL NOT NULL DEFAULT 0,
		lon REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS webhook_configs (
		id TEXT PRIMARY KEY,
		url TEXT NOT NULL,
		secret TEXT NOT NULL DEFAULT '',
		events TEXT NOT NULL DEFAULT '[]',
		max_retries INTEGER NOT NULL DEFAULT 3,
		timeout_sec INTEGER NOT NULL DEFAULT 10,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS delivery_logs (
		id TEXT PRIMARY KEY,
		webhook_id TEXT NOT NULL,
		event TEXT NOT NULL,
		device_imei TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		attempt INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS positions (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL REFERENCES devices(imei) ON DELETE CASCADE,
		lat REAL NOT NULL,
		lon REAL NOT NULL,
		alt REAL NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT 'gps',
		cep REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_positions_device ON positions(device_imei, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_device ON messages(device_imei, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		id TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
}

// --- Devices ---

func (d *DB) CreateDevice(ctx context.Context, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO devices (imei, label, type, notes) VALUES (?, ?, ?, ?)",
		dev.IMEI, dev.Label, dev.Type, dev.Notes)
	return err
}

func (d *DB) GetDevice(ctx context.Context, imei string) (*store.Device, error) {
	var dev store.Device
	var lastSeen, createdAt, updatedAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE imei=?", imei,
	).Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &lastSeen, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	dev.LastSeen, _ = time.Parse(time.DateTime, lastSeen)
	dev.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	dev.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt)
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices ORDER BY label, imei")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		var lastSeen, createdAt, updatedAt string
		if err := rows.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &lastSeen, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		dev.LastSeen, _ = time.Parse(time.DateTime, lastSeen)
		dev.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		dev.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt)
		devices = append(devices, dev)
	}
	return devices, nil
}

func (d *DB) UpdateDevice(ctx context.Context, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET label=?, type=?, notes=?, updated_at=datetime('now') WHERE imei=?",
		dev.Label, dev.Type, dev.Notes, dev.IMEI)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, imei string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM devices WHERE imei=?", imei)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, imei string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE devices SET last_seen=datetime('now') WHERE imei=?", imei)
	return err
}

// --- Messages ---

func (d *DB) InsertMessage(ctx context.Context, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		boolToInt(m.Compressed), m.Status, m.Error, m.Lat, m.Lon)
	return err
}

func (d *DB) ListMessages(ctx context.Context, deviceIMEI string, limit int) ([]store.Message, error) {
	query := "SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages"
	var args []interface{}
	if deviceIMEI != "" {
		query += " WHERE device_imei=?"
		args = append(args, deviceIMEI)
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []store.Message
	for rows.Next() {
		var m store.Message
		var compressed int
		var createdAt string
		if err := rows.Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN,
			&m.Text, &m.RawHex, &compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &createdAt); err != nil {
			return nil, err
		}
		m.Compressed = compressed != 0
		m.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (d *DB) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	var m store.Message
	var compressed int
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE id=?", id,
	).Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN, &m.Text, &m.RawHex,
		&compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &createdAt)
	if err != nil {
		return nil, err
	}
	m.Compressed = compressed != 0
	m.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &m, nil
}

// --- Webhooks ---

func (d *DB) SaveWebhook(ctx context.Context, w *store.WebhookConfig) error {
	eventsJSON, _ := json.Marshal(w.Events)
	_, err := d.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.URL, w.Secret, string(eventsJSON), w.MaxRetries, w.TimeoutSec, boolToInt(w.Enabled))
	return err
}

func (d *DB) ListWebhooks(ctx context.Context) ([]store.WebhookConfig, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var webhooks []store.WebhookConfig
	for rows.Next() {
		var w store.WebhookConfig
		var eventsStr string
		var enabled int
		var createdAt string
		if err := rows.Scan(&w.ID, &w.URL, &w.Secret, &eventsStr, &w.MaxRetries, &w.TimeoutSec, &enabled, &createdAt); err != nil {
			return nil, err
		}
		w.Enabled = enabled != 0
		w.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		_ = json.Unmarshal([]byte(eventsStr), &w.Events)
		webhooks = append(webhooks, w)
	}
	return webhooks, nil
}

func (d *DB) DeleteWebhook(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM webhook_configs WHERE id=?", id)
	return err
}

// --- Delivery logs ---

func (d *DB) InsertDeliveryLog(ctx context.Context, l *store.DeliveryLog) error {
	if l.ID == "" {
		l.ID = fmt.Sprintf("dl-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO delivery_logs (id, webhook_id, event, device_imei, status_code, error, attempt)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.WebhookID, l.Event, l.DeviceIMEI, l.StatusCode, l.Error, l.Attempt)
	return err
}

func (d *DB) ListDeliveryLogs(ctx context.Context, limit int) ([]store.DeliveryLog, error) {
	query := "SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var logs []store.DeliveryLog
	for rows.Next() {
		var l store.DeliveryLog
		var createdAt string
		if err := rows.Scan(&l.ID, &l.WebhookID, &l.Event, &l.DeviceIMEI, &l.StatusCode, &l.Error, &l.Attempt, &createdAt); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		logs = append(logs, l)
	}
	return logs, nil
}

// --- Positions ---

func (d *DB) InsertPosition(ctx context.Context, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO positions (id, device_imei, lat, lon, alt, source, cep) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Source, p.CEP)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=? ORDER BY rowid DESC LIMIT 1",
		deviceIMEI,
	).Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Source, &p.CEP, &createdAt)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, deviceIMEI string, limit int) ([]store.Position, error) {
	query := "SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=? ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, deviceIMEI)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		var createdAt string
		if err := rows.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Source, &p.CEP, &createdAt); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		positions = append(positions, p)
	}
	return positions, nil
}

// --- Audit log ---

func (d *DB) InsertAuditEntry(ctx context.Context, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip) VALUES (?, ?, ?, ?, ?)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP)
	return err
}

func (d *DB) ListAuditEntries(ctx context.Context, limit int) ([]store.AuditEntry, error) {
	query := "SELECT id, action, actor, detail, ip, created_at FROM audit_log ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		var createdAt string
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &createdAt); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		entries = append(entries, a)
	}
	return entries, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Ensure DB implements Store at compile time.
var _ store.Store = (*DB)(nil)

// suppress unused import warning
var _ = strings.HasPrefix
