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
	// Run ALTER TABLE migrations separately — ignore "duplicate column" errors
	// for idempotency (SQLite has no ADD COLUMN IF NOT EXISTS).
	for _, m := range alterMigrations {
		if _, err := d.db.ExecContext(ctx, m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("alter migration: %w", err)
			}
		}
	}
	// Run post-alter index/migration statements (idempotent via IF NOT EXISTS).
	for i, m := range postAlterMigrations {
		if _, err := d.db.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("post-alter migration %d: %w", i+1, err)
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

// alterMigrations add tenant_id to existing tables. These use ALTER TABLE
// which cannot be made idempotent in SQLite, so errors for duplicate columns
// are ignored by the Migrate function.
var alterMigrations = []string{
	`ALTER TABLE devices ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE messages ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE webhook_configs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE delivery_logs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE positions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE audit_log ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE audit_log ADD COLUMN prev_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE audit_log ADD COLUMN hash TEXT NOT NULL DEFAULT ''`,
	// v0.4: extended position fields
	`ALTER TABLE positions ADD COLUMN speed REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE positions ADD COLUMN heading REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE positions ADD COLUMN sats INTEGER NOT NULL DEFAULT 0`,
}

// postAlterMigrations create indexes and new tables. Safe to re-run.
var postAlterMigrations = []string{
	`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		key_hash TEXT NOT NULL UNIQUE,
		key_prefix TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT 'viewer',
		label TEXT NOT NULL DEFAULT '',
		device_imei TEXT NOT NULL DEFAULT '',
		last_used TEXT NOT NULL DEFAULT '',
		expires_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		tenant_id TEXT NOT NULL DEFAULT 'default'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id)`,
	`CREATE TABLE IF NOT EXISTS device_configs (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1,
		config TEXT NOT NULL DEFAULT '{}',
		author TEXT NOT NULL DEFAULT '',
		comment TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		tenant_id TEXT NOT NULL DEFAULT 'default',
		UNIQUE(device_imei, version, tenant_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_configs_device ON device_configs(device_imei, tenant_id, version DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_devices_tenant ON devices(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_tenant ON messages(tenant_id, device_imei)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_configs_tenant ON webhook_configs(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_positions_tenant ON positions(tenant_id, device_imei)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_log_tenant ON audit_log(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_logs_tenant ON delivery_logs(tenant_id)`,
	// Escalation chains (v0.3)
	`CREATE TABLE IF NOT EXISTS escalation_chains (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		tiers TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		tenant_id TEXT NOT NULL DEFAULT 'default'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_escalation_chains_tenant ON escalation_chains(tenant_id)`,
	// Alerts (v0.3)
	`CREATE TABLE IF NOT EXISTS alerts (
		id TEXT PRIMARY KEY,
		chain_id TEXT NOT NULL DEFAULT '',
		device_imei TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'sos',
		detail TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'triggered',
		current_tier INTEGER NOT NULL DEFAULT 0,
		retries INTEGER NOT NULL DEFAULT 0,
		acked_by TEXT NOT NULL DEFAULT '',
		acked_at TEXT NOT NULL DEFAULT '',
		next_esc_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		tenant_id TEXT NOT NULL DEFAULT 'default'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_alerts_tenant ON alerts(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_alerts_state ON alerts(state, next_esc_at)`,
	// Notification preferences (v0.3 — Apprise integration, MESHSAT-112)
	`CREATE TABLE IF NOT EXISTS notification_prefs (
		device_imei TEXT NOT NULL DEFAULT '*',
		urls TEXT NOT NULL DEFAULT '[]',
		events TEXT NOT NULL DEFAULT '[]',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		tenant_id TEXT NOT NULL DEFAULT 'default',
		PRIMARY KEY (device_imei, tenant_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notification_prefs_tenant ON notification_prefs(tenant_id)`,
}

// --- Devices ---

func (d *DB) CreateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO devices (imei, label, type, notes, tenant_id) VALUES (?, ?, ?, ?, ?)",
		dev.IMEI, dev.Label, dev.Type, dev.Notes, tenantID)
	return err
}

func (d *DB) GetDevice(ctx context.Context, tenantID string, imei string) (*store.Device, error) {
	var dev store.Device
	var lastSeen, createdAt, updatedAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE imei=? AND tenant_id=?", imei, tenantID,
	).Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &lastSeen, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	dev.LastSeen, _ = time.Parse(time.DateTime, lastSeen)
	dev.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	dev.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt)
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context, tenantID string) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE tenant_id=? ORDER BY label, imei", tenantID)
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

func (d *DB) UpdateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET label=?, type=?, notes=?, updated_at=datetime('now') WHERE imei=? AND tenant_id=?",
		dev.Label, dev.Type, dev.Notes, dev.IMEI, tenantID)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM devices WHERE imei=? AND tenant_id=?", imei, tenantID)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE devices SET last_seen=datetime('now') WHERE imei=? AND tenant_id=?", imei, tenantID)
	return err
}

// --- Messages ---

func (d *DB) InsertMessage(ctx context.Context, tenantID string, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		boolToInt(m.Compressed), m.Status, m.Error, m.Lat, m.Lon, tenantID)
	return err
}

func (d *DB) ListMessages(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Message, error) {
	query := "SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE tenant_id=?"
	args := []interface{}{tenantID}
	if deviceIMEI != "" {
		query += " AND device_imei=?"
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

func (d *DB) GetMessage(ctx context.Context, tenantID string, id string) (*store.Message, error) {
	var m store.Message
	var compressed int
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE id=? AND tenant_id=?", id, tenantID,
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

func (d *DB) SaveWebhook(ctx context.Context, tenantID string, w *store.WebhookConfig) error {
	eventsJSON, _ := json.Marshal(w.Events)
	_, err := d.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.URL, w.Secret, string(eventsJSON), w.MaxRetries, w.TimeoutSec, boolToInt(w.Enabled), tenantID)
	return err
}

func (d *DB) ListWebhooks(ctx context.Context, tenantID string) ([]store.WebhookConfig, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs WHERE tenant_id=?", tenantID)
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

func (d *DB) DeleteWebhook(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM webhook_configs WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Delivery logs ---

func (d *DB) InsertDeliveryLog(ctx context.Context, tenantID string, l *store.DeliveryLog) error {
	if l.ID == "" {
		l.ID = fmt.Sprintf("dl-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO delivery_logs (id, webhook_id, event, device_imei, status_code, error, attempt, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.WebhookID, l.Event, l.DeviceIMEI, l.StatusCode, l.Error, l.Attempt, tenantID)
	return err
}

func (d *DB) ListDeliveryLogs(ctx context.Context, tenantID string, limit int) ([]store.DeliveryLog, error) {
	query := "SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs WHERE tenant_id=? ORDER BY created_at DESC"
	args := []interface{}{tenantID}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
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

func (d *DB) InsertPosition(ctx context.Context, tenantID string, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO positions (id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Speed, p.Heading, p.Sats, p.Source, p.CEP, tenantID)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, tenantID string, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at FROM positions WHERE device_imei=? AND tenant_id=? ORDER BY rowid DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &createdAt)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Position, error) {
	query := "SELECT id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at FROM positions WHERE device_imei=? AND tenant_id=? ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, deviceIMEI, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		var createdAt string
		if err := rows.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &createdAt); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		positions = append(positions, p)
	}
	return positions, nil
}

// --- Audit log ---

func (d *DB) InsertAuditEntry(ctx context.Context, tenantID string, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip, prev_hash, hash, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP, a.PrevHash, a.Hash, tenantID)
	return err
}

func (d *DB) ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]store.AuditEntry, error) {
	query := "SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=? ORDER BY rowid DESC"
	args := []interface{}{tenantID}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		var createdAt string
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &createdAt); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		entries = append(entries, a)
	}
	return entries, nil
}

func (d *DB) GetLatestAuditEntry(ctx context.Context, tenantID string) (*store.AuditEntry, error) {
	var a store.AuditEntry
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=? ORDER BY rowid DESC LIMIT 1",
		tenantID,
	).Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &createdAt)
	if err != nil {
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &a, nil
}

// --- Device config versioning ---

func (d *DB) CreateDeviceConfig(ctx context.Context, tenantID string, c *store.DeviceConfig) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("cfg-%d", time.Now().UnixNano())
	}
	// Auto-increment version: max(version) + 1 for this device+tenant.
	var maxVersion int
	err := d.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM device_configs WHERE device_imei=? AND tenant_id=?",
		c.DeviceIMEI, tenantID,
	).Scan(&maxVersion)
	if err != nil {
		return fmt.Errorf("get max version: %w", err)
	}
	c.Version = maxVersion + 1

	_, err = d.db.ExecContext(ctx,
		`INSERT INTO device_configs (id, device_imei, version, config, author, comment, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.DeviceIMEI, c.Version, c.Config, c.Author, c.Comment, tenantID)
	return err
}

func (d *DB) GetDeviceConfigLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? ORDER BY version DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &c, nil
}

func (d *DB) GetDeviceConfigVersion(ctx context.Context, tenantID string, deviceIMEI string, version int) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? AND version=?",
		deviceIMEI, tenantID, version,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &c, nil
}

func (d *DB) ListDeviceConfigVersions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.DeviceConfig, error) {
	query := "SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? ORDER BY version DESC"
	args := []interface{}{deviceIMEI, tenantID}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var configs []store.DeviceConfig
	for rows.Next() {
		var c store.DeviceConfig
		var createdAt string
		if err := rows.Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &createdAt); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		configs = append(configs, c)
	}
	return configs, nil
}

// --- API keys ---

func (d *DB) CreateAPIKey(ctx context.Context, tenantID string, k *store.APIKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("key-%d", time.Now().UnixNano())
	}
	var expiresAt string
	if !k.ExpiresAt.IsZero() {
		expiresAt = k.ExpiresAt.Format(time.DateTime)
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, key_hash, key_prefix, role, label, device_imei, expires_at, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.KeyPrefix, k.Role, k.Label, k.DeviceIMEI, expiresAt, tenantID)
	return err
}

func (d *DB) GetAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKey, string, error) {
	var k store.APIKey
	var tenantID, lastUsed, expiresAt, createdAt string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, created_at, tenant_id FROM api_keys WHERE key_hash=?",
		keyHash,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &lastUsed, &expiresAt, &createdAt, &tenantID)
	if err != nil {
		return nil, "", err
	}
	k.LastUsed, _ = time.Parse(time.DateTime, lastUsed)
	k.ExpiresAt, _ = time.Parse(time.DateTime, expiresAt)
	k.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &k, tenantID, nil
}

func (d *DB) ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, created_at FROM api_keys WHERE tenant_id=? ORDER BY created_at DESC",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		var lastUsed, expiresAt, createdAt string
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &lastUsed, &expiresAt, &createdAt); err != nil {
			return nil, err
		}
		k.LastUsed, _ = time.Parse(time.DateTime, lastUsed)
		k.ExpiresAt, _ = time.Parse(time.DateTime, expiresAt)
		k.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		keys = append(keys, k)
	}
	return keys, nil
}

func (d *DB) DeleteAPIKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM api_keys WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

func (d *DB) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE api_keys SET last_used=datetime('now') WHERE id=?", id)
	return err
}

// --- Notification Preferences ---

func (d *DB) SaveNotificationPref(ctx context.Context, tenantID string, p *store.NotificationPref) error {
	urlsJSON, _ := json.Marshal(p.URLs)
	eventsJSON, _ := json.Marshal(p.Events)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO notification_prefs (device_imei, urls, events, enabled, tenant_id, updated_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT (device_imei, tenant_id) DO UPDATE SET
		   urls=excluded.urls, events=excluded.events, enabled=excluded.enabled, updated_at=datetime('now')`,
		p.DeviceIMEI, string(urlsJSON), string(eventsJSON), boolToInt(p.Enabled), tenantID)
	return err
}

func (d *DB) GetNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) (*store.NotificationPref, error) {
	var p store.NotificationPref
	var urlsStr, eventsStr, createdAt, updatedAt string
	var enabled int
	err := d.db.QueryRowContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE device_imei=? AND tenant_id=?",
		deviceIMEI, tenantID,
	).Scan(&p.DeviceIMEI, &urlsStr, &eventsStr, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(urlsStr), &p.URLs)
	_ = json.Unmarshal([]byte(eventsStr), &p.Events)
	p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	p.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt)
	return &p, nil
}

func (d *DB) ListNotificationPrefs(ctx context.Context, tenantID string) ([]store.NotificationPref, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE tenant_id=? ORDER BY device_imei",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var prefs []store.NotificationPref
	for rows.Next() {
		var p store.NotificationPref
		var urlsStr, eventsStr, createdAt, updatedAt string
		var enabled int
		if err := rows.Scan(&p.DeviceIMEI, &urlsStr, &eventsStr, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		_ = json.Unmarshal([]byte(urlsStr), &p.URLs)
		_ = json.Unmarshal([]byte(eventsStr), &p.Events)
		p.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
		p.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt)
		prefs = append(prefs, p)
	}
	return prefs, rows.Err()
}

func (d *DB) DeleteNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM notification_prefs WHERE device_imei=? AND tenant_id=?", deviceIMEI, tenantID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- Escalation Chains ---

func (d *DB) CreateEscalationChain(ctx context.Context, tenantID string, c *store.EscalationChain) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("chain-%d", time.Now().UnixNano())
	}
	tiersJSON, err := json.Marshal(c.Tiers)
	if err != nil {
		return fmt.Errorf("marshal tiers: %w", err)
	}
	_, err = d.db.ExecContext(ctx,
		"INSERT INTO escalation_chains (id, name, tiers, tenant_id) VALUES (?, ?, ?, ?)",
		c.ID, c.Name, string(tiersJSON), tenantID)
	return err
}

func (d *DB) GetEscalationChain(ctx context.Context, tenantID string, id string) (*store.EscalationChain, error) {
	var c store.EscalationChain
	var tiersJSON, createdAt, updatedAt string
	query := "SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE id=?"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(
		&c.ID, &c.Name, &tiersJSON, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tiersJSON), &c.Tiers)
	c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	c.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &c, nil
}

func (d *DB) ListEscalationChains(ctx context.Context, tenantID string) ([]store.EscalationChain, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE tenant_id=? ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var chains []store.EscalationChain
	for rows.Next() {
		var c store.EscalationChain
		var tiersJSON, createdAt, updatedAt string
		if err := rows.Scan(&c.ID, &c.Name, &tiersJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tiersJSON), &c.Tiers)
		c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		c.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
		chains = append(chains, c)
	}
	return chains, rows.Err()
}

func (d *DB) DeleteEscalationChain(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM escalation_chains WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Alerts ---

func (d *DB) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO alerts (id, chain_id, device_imei, type, detail, state, current_tier, retries,
			acked_by, acked_at, next_esc_at, created_at, updated_at, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ChainID, a.DeviceIMEI, a.Type, a.Detail, a.State, a.CurrentTier, a.Retries,
		a.AckedBy, fmtTime(a.AckedAt), fmtTime(a.NextEscAt),
		fmtTime(a.CreatedAt), fmtTime(a.UpdatedAt), tenantID)
	return err
}

func (d *DB) GetAlert(ctx context.Context, tenantID string, id string) (*store.Alert, error) {
	var a store.Alert
	var ackedAt, nextEscAt, createdAt, updatedAt string
	query := "SELECT id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts WHERE id=?"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(
		&a.ID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
		&a.CurrentTier, &a.Retries, &a.AckedBy, &ackedAt, &nextEscAt, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	a.AckedAt, _ = time.Parse("2006-01-02 15:04:05", ackedAt)
	a.NextEscAt, _ = time.Parse("2006-01-02 15:04:05", nextEscAt)
	a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	a.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &a, nil
}

func (d *DB) ListAlerts(ctx context.Context, tenantID string, activeOnly bool, limit int) ([]store.Alert, error) {
	query := "SELECT id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts"
	var args []interface{}
	var conditions []string
	if tenantID != "" {
		conditions = append(conditions, "tenant_id=?")
		args = append(args, tenantID)
	}
	if activeOnly {
		conditions = append(conditions, "state IN ('triggered', 'escalating')")
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var alerts []store.Alert
	for rows.Next() {
		var a store.Alert
		var ackedAt, nextEscAt, createdAt, updatedAt string
		if err := rows.Scan(
			&a.ID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
			&a.CurrentTier, &a.Retries, &a.AckedBy, &ackedAt, &nextEscAt, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		a.AckedAt, _ = time.Parse("2006-01-02 15:04:05", ackedAt)
		a.NextEscAt, _ = time.Parse("2006-01-02 15:04:05", nextEscAt)
		a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		a.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (d *DB) UpdateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	query := `UPDATE alerts SET state=?, current_tier=?, retries=?, acked_by=?, acked_at=?,
		next_esc_at=?, updated_at=? WHERE id=?`
	args := []interface{}{
		a.State, a.CurrentTier, a.Retries, a.AckedBy, fmtTime(a.AckedAt),
		fmtTime(a.NextEscAt), fmtTime(a.UpdatedAt), a.ID,
	}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	_, err := d.db.ExecContext(ctx, query, args...)
	return err
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// Ensure DB implements Store at compile time.
var _ store.Store = (*DB)(nil)

// suppress unused import warning
var _ = strings.HasPrefix
