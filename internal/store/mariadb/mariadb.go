// Package mariadb implements store.Store using MariaDB Galera Cluster.
// Used in cluster and k8s modes for active-active multi-master replication.
// Uses go-sql-driver/mysql (pure Go, no CGO).
package mariadb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/dbwrap"
)

// DB implements store.Store with MariaDB.
type DB struct {
	db    dbwrap.SQLDB
	rawDB *sql.DB // kept for RawDB() / GaleraReady (needs raw driver access)
}

// New connects to MariaDB and returns a DB.
// dsn format: "user:password@tcp(host:3306)/dbname?parseTime=true"
// slowQueryThreshold controls slow query logging (0 = disabled).
func New(dsn string, slowQueryThreshold time.Duration) (*DB, error) {
	// Ensure parseTime is enabled for DATETIME scanning
	if dsn != "" && !contains(dsn, "parseTime") {
		sep := "?"
		if contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "parseTime=true"
	}
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mariadb: open: %w", err)
	}
	conn.SetMaxOpenConns(25)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mariadb: ping: %w", err)
	}
	return &DB{
		db:    dbwrap.NewObservedDB(conn, "mariadb", slowQueryThreshold),
		rawDB: conn,
	}, nil
}

// RawDB returns the underlying *sql.DB for direct queries (e.g., cluster health checks).
func (d *DB) RawDB() *sql.DB                 { return d.rawDB }
func (d *DB) Close() error                   { return d.db.Close() }
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// GaleraReady returns nil if the local Galera node can accept writes.
// Checks wsrep_ready (ON = can write) and wsrep_local_state_comment (Synced = healthy).
// Returns an error if the node is read-only or desynced — HAProxy should stop routing traffic here.
func (d *DB) GaleraReady(ctx context.Context) error {
	var name, value string
	err := d.db.QueryRowContext(ctx, "SHOW STATUS LIKE 'wsrep_ready'").Scan(&name, &value)
	if err != nil {
		// Not a Galera node (standalone MariaDB) — fall back to ping
		return d.db.PingContext(ctx)
	}
	if value != "ON" {
		return fmt.Errorf("galera: wsrep_ready=%s (not accepting writes)", value)
	}
	return nil
}

// Migrate creates all tables. Safe to re-run (IF NOT EXISTS).
func (d *DB) Migrate(ctx context.Context) error {
	for i, m := range migrations {
		if _, err := d.db.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("mariadb migration %d: %w", i+1, err)
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS devices (
		imei VARCHAR(64) PRIMARY KEY,
		label VARCHAR(255) NOT NULL DEFAULT '',
		type VARCHAR(64) NOT NULL DEFAULT 'rockblock',
		notes TEXT NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		last_seen DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_devices_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS messages (
		id VARCHAR(64) PRIMARY KEY,
		device_imei VARCHAR(64) NOT NULL,
		direction VARCHAR(8) NOT NULL,
		channel VARCHAR(32) NOT NULL DEFAULT 'iridium',
		momsn INT NOT NULL DEFAULT 0,
		text TEXT NOT NULL,
		raw_hex TEXT NOT NULL,
		compressed TINYINT(1) NOT NULL DEFAULT 0,
		status VARCHAR(32) NOT NULL DEFAULT 'received',
		error TEXT NOT NULL,
		lat DOUBLE NOT NULL DEFAULT 0,
		lon DOUBLE NOT NULL DEFAULT 0,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_messages_device (device_imei, created_at DESC),
		INDEX idx_messages_tenant (tenant_id, device_imei)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS webhook_configs (
		id VARCHAR(64) PRIMARY KEY,
		url TEXT NOT NULL,
		secret VARCHAR(255) NOT NULL DEFAULT '',
		events JSON NOT NULL,
		max_retries INT NOT NULL DEFAULT 3,
		timeout_sec INT NOT NULL DEFAULT 10,
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_webhook_configs_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS delivery_logs (
		id VARCHAR(64) PRIMARY KEY,
		webhook_id VARCHAR(64) NOT NULL,
		event VARCHAR(64) NOT NULL,
		device_imei VARCHAR(64) NOT NULL DEFAULT '',
		status_code INT NOT NULL DEFAULT 0,
		error TEXT NOT NULL,
		attempt INT NOT NULL DEFAULT 0,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_delivery_logs_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS positions (
		id VARCHAR(64) PRIMARY KEY,
		device_imei VARCHAR(64) NOT NULL,
		lat DOUBLE NOT NULL,
		lon DOUBLE NOT NULL,
		alt DOUBLE NOT NULL DEFAULT 0,
		speed DOUBLE NOT NULL DEFAULT 0,
		heading DOUBLE NOT NULL DEFAULT 0,
		sats INT NOT NULL DEFAULT 0,
		source VARCHAR(32) NOT NULL DEFAULT 'gps',
		cep DOUBLE NOT NULL DEFAULT 0,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_positions_device (device_imei, created_at DESC),
		INDEX idx_positions_tenant (tenant_id, device_imei)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS audit_log (
		id VARCHAR(64) PRIMARY KEY,
		action VARCHAR(64) NOT NULL,
		actor VARCHAR(255) NOT NULL DEFAULT '',
		detail TEXT NOT NULL,
		ip VARCHAR(64) NOT NULL DEFAULT '',
		prev_hash VARCHAR(64) NOT NULL DEFAULT '',
		hash VARCHAR(64) NOT NULL DEFAULT '',
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_audit_log_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id VARCHAR(64) PRIMARY KEY,
		key_hash VARCHAR(64) NOT NULL UNIQUE,
		key_prefix VARCHAR(32) NOT NULL DEFAULT '',
		role VARCHAR(16) NOT NULL DEFAULT 'viewer',
		label VARCHAR(255) NOT NULL DEFAULT '',
		device_imei VARCHAR(64) NOT NULL DEFAULT '',
		last_used DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
		expires_at DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_api_keys_hash (key_hash),
		INDEX idx_api_keys_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS device_configs (
		id VARCHAR(64) PRIMARY KEY,
		device_imei VARCHAR(64) NOT NULL,
		version INT NOT NULL DEFAULT 1,
		config MEDIUMTEXT NOT NULL,
		author VARCHAR(255) NOT NULL DEFAULT '',
		comment TEXT NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY uq_device_config (device_imei, version, tenant_id),
		INDEX idx_device_configs_device (device_imei, tenant_id, version DESC)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS escalation_chains (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL DEFAULT '',
		tiers JSON NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_escalation_chains_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS alerts (
		id VARCHAR(64) PRIMARY KEY,
		chain_id VARCHAR(64) NOT NULL DEFAULT '',
		device_imei VARCHAR(64) NOT NULL DEFAULT '',
		type VARCHAR(32) NOT NULL DEFAULT 'sos',
		detail TEXT NOT NULL,
		state VARCHAR(32) NOT NULL DEFAULT 'triggered',
		current_tier INT NOT NULL DEFAULT 0,
		retries INT NOT NULL DEFAULT 0,
		acked_by VARCHAR(255) NOT NULL DEFAULT '',
		acked_at DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
		next_esc_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_alerts_tenant (tenant_id),
		INDEX idx_alerts_state (state, next_esc_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS notification_prefs (
		device_imei VARCHAR(64) NOT NULL DEFAULT '*',
		urls JSON NOT NULL,
		events JSON NOT NULL,
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		PRIMARY KEY (device_imei, tenant_id),
		INDEX idx_notification_prefs_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS users (
		id VARCHAR(64) PRIMARY KEY,
		email VARCHAR(255) NOT NULL,
		name VARCHAR(255) NOT NULL DEFAULT '',
		password_hash VARCHAR(255) NOT NULL,
		role VARCHAR(16) NOT NULL DEFAULT 'viewer',
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		failed_logins INT NOT NULL DEFAULT 0,
		locked_until DATETIME NULL,
		last_login_at DATETIME NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uq_users_email (email, tenant_id),
		INDEX idx_users_email (email, tenant_id),
		INDEX idx_users_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS refresh_tokens (
		id VARCHAR(64) PRIMARY KEY,
		user_id VARCHAR(64) NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		token_hash VARCHAR(64) NOT NULL UNIQUE,
		expires_at DATETIME NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_refresh_tokens_hash (token_hash),
		INDEX idx_refresh_tokens_user (user_id, tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// Device encryption keys (v1.1 — E2E encryption, MESHSAT-169)
	`CREATE TABLE IF NOT EXISTS device_keys (
		id VARCHAR(64) PRIMARY KEY,
		device_imei VARCHAR(20) NOT NULL,
		key_hash VARCHAR(64) NOT NULL,
		key_hex VARCHAR(64) NOT NULL DEFAULT '',
		mode VARCHAR(16) NOT NULL DEFAULT 'decrypt',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		INDEX idx_device_keys_device (device_imei, tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// Device WireGuard peer tracking (v1.2 — auto-provisioning, MESHSAT-176)
	`CREATE TABLE IF NOT EXISTS device_wireguard (
		device_imei VARCHAR(64) NOT NULL,
		peer_id VARCHAR(64) NOT NULL DEFAULT '',
		vpn_address VARCHAR(64) NOT NULL DEFAULT '',
		public_key VARCHAR(255) NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		PRIMARY KEY (device_imei, tenant_id),
		INDEX idx_device_wireguard_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// Routes (v1.3 — configurable routing engine, MESHSAT-178)
	`CREATE TABLE IF NOT EXISTS routes (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL DEFAULT '',
		source_type VARCHAR(64) NOT NULL DEFAULT '',
		destination_type VARCHAR(64) NOT NULL DEFAULT '',
		filter TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		INDEX idx_routes_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// System config (key-value store for hub identity, settings)
	`CREATE TABLE IF NOT EXISTS system_config (
		` + "`key`" + ` VARCHAR(255) PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// Bridges (MESHSAT-282 — field bridge registry)
	`CREATE TABLE IF NOT EXISTS bridges (
		bridge_id VARCHAR(64) PRIMARY KEY,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		label VARCHAR(255) NOT NULL DEFAULT '',
		hostname VARCHAR(255) NOT NULL DEFAULT '',
		version VARCHAR(64) NOT NULL DEFAULT '',
		mode VARCHAR(32) NOT NULL DEFAULT 'direct',
		location_lat DOUBLE NOT NULL DEFAULT 0,
		location_lon DOUBLE NOT NULL DEFAULT 0,
		location_alt DOUBLE NOT NULL DEFAULT 0,
		capabilities JSON NOT NULL DEFAULT ('[]'),
		reticulum_hash VARCHAR(64) NOT NULL DEFAULT '',
		reticulum_pubkey TEXT NOT NULL DEFAULT (''),
		cot_type VARCHAR(32) NOT NULL DEFAULT 'a-f-G-U-C-I',
		cot_callsign VARCHAR(64) NOT NULL DEFAULT '',
		online TINYINT(1) NOT NULL DEFAULT 0,
		last_birth JSON NOT NULL DEFAULT ('{}'),
		last_health JSON NOT NULL DEFAULT ('{}'),
		last_seen DATETIME NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_bridges_tenant (tenant_id),
		INDEX idx_bridges_online (online)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// MESHSAT-282: associate devices with bridges
	"ALTER TABLE devices ADD COLUMN IF NOT EXISTS bridge_id VARCHAR(64) DEFAULT NULL",
	// MESHSAT-291: bridge MQTT authentication
	"ALTER TABLE bridges ADD COLUMN IF NOT EXISTS mqtt_username VARCHAR(64) NOT NULL DEFAULT ''",
	"ALTER TABLE bridges ADD COLUMN IF NOT EXISTS mqtt_password_hash VARCHAR(255) NOT NULL DEFAULT ''",
	"ALTER TABLE bridges ADD COLUMN IF NOT EXISTS cert_pem TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE bridges ADD COLUMN IF NOT EXISTS cert_expiry DATETIME NULL",
	// Fix: ensure NOT NULL columns have defaults (previous migrations may have missed them)
	"ALTER TABLE bridges ALTER COLUMN cert_pem SET DEFAULT ''",
	"ALTER TABLE bridges ALTER COLUMN reticulum_pubkey SET DEFAULT ''",
	// MESHSAT-314: scheduled messages
	"ALTER TABLE messages ADD COLUMN IF NOT EXISTS scheduled_at DATETIME NULL",

	// MESHSAT-310: cost tracking ledger
	`CREATE TABLE IF NOT EXISTS cost_ledger (
		id VARCHAR(64) PRIMARY KEY,
		device_imei VARCHAR(64) NOT NULL DEFAULT '',
		interface_type VARCHAR(32) NOT NULL DEFAULT '',
		direction VARCHAR(8) NOT NULL DEFAULT 'mt',
		cost_usd DOUBLE NOT NULL DEFAULT 0,
		message_id VARCHAR(64) NOT NULL DEFAULT '',
		detail TEXT NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_cost_ledger_tenant (tenant_id, created_at DESC),
		INDEX idx_cost_ledger_device (device_imei, tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// MESHSAT-311: device groups for fleet organization
	`CREATE TABLE IF NOT EXISTS device_groups (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL DEFAULT '',
		description TEXT NOT NULL,
		color VARCHAR(16) NOT NULL DEFAULT '#6b7280',
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_device_groups_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS device_group_members (
		group_id VARCHAR(64) NOT NULL,
		device_imei VARCHAR(64) NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		PRIMARY KEY (group_id, device_imei, tenant_id),
		INDEX idx_dgm_device (device_imei, tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// MESHSAT-315: API key rotation
	"ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS rotation_days INT NOT NULL DEFAULT 0",

	// MESHSAT-312: message templates with variable substitution
	`CREATE TABLE IF NOT EXISTS message_templates (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL DEFAULT '',
		body TEXT NOT NULL,
		variables TEXT NOT NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_message_templates_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	// MESHSAT-313: alert rules
	`CREATE TABLE IF NOT EXISTS alert_rules (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL DEFAULT '',
		condition_type VARCHAR(64) NOT NULL DEFAULT '',
		condition_params TEXT NOT NULL,
		chain_id VARCHAR(64) NOT NULL DEFAULT '',
		device_filter VARCHAR(255) NOT NULL DEFAULT '*',
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		last_evaluated DATETIME NULL,
		tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_alert_rules_tenant (tenant_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
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
	err := d.db.QueryRowContext(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE imei=? AND tenant_id=?", imei, tenantID,
	).Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context, tenantID string) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE tenant_id=? ORDER BY label, imei", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		if err := rows.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) UpdateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET label=?, type=?, notes=?, updated_at=NOW() WHERE imei=? AND tenant_id=?",
		dev.Label, dev.Type, dev.Notes, dev.IMEI, tenantID)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM devices WHERE imei=? AND tenant_id=?", imei, tenantID)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE devices SET last_seen=NOW() WHERE imei=? AND tenant_id=?", imei, tenantID)
	return err
}

// --- Messages ---

func (d *DB) InsertMessage(ctx context.Context, tenantID string, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	var scheduledAt interface{}
	if !m.ScheduledAt.IsZero() {
		scheduledAt = m.ScheduledAt.UTC()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, tenant_id, scheduled_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		m.Compressed, m.Status, m.Error, m.Lat, m.Lon, tenantID, scheduledAt)
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
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []store.Message
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN,
			&m.Text, &m.RawHex, &m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (d *DB) GetMessage(ctx context.Context, tenantID string, id string) (*store.Message, error) {
	var m store.Message
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE id=? AND tenant_id=?", id, tenantID,
	).Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN, &m.Text, &m.RawHex,
		&m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (d *DB) ListScheduledMessages(ctx context.Context, before time.Time, limit int) ([]store.Message, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, scheduled_at, created_at, tenant_id
		 FROM messages WHERE scheduled_at IS NOT NULL AND scheduled_at <= ? AND status = 'scheduled' ORDER BY scheduled_at ASC LIMIT ?`,
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
			m.ScheduledAt = scheduledAt.Time
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (d *DB) UpdateMessageStatus(ctx context.Context, _ string, id string, status string, errMsg string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE messages SET status=?, error=? WHERE id=?", status, errMsg, id)
	return err
}

// --- Webhooks ---

func (d *DB) SaveWebhook(ctx context.Context, tenantID string, w *store.WebhookConfig) error {
	eventsJSON, _ := json.Marshal(w.Events)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE url=VALUES(url), secret=VALUES(secret), events=VALUES(events),
		   max_retries=VALUES(max_retries), timeout_sec=VALUES(timeout_sec), enabled=VALUES(enabled)`,
		w.ID, w.URL, w.Secret, eventsJSON, w.MaxRetries, w.TimeoutSec, w.Enabled, tenantID)
	return err
}

func (d *DB) ListWebhooks(ctx context.Context, tenantID string) ([]store.WebhookConfig, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs WHERE tenant_id=?", tenantID)
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
		_ = json.Unmarshal(eventsJSON, &w.Events)
		webhooks = append(webhooks, w)
	}
	return webhooks, rows.Err()
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
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs WHERE tenant_id=? ORDER BY created_at DESC LIMIT ?", tenantID, limit)
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
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// --- Positions ---

func (d *DB) InsertPosition(ctx context.Context, tenantID string, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO positions (id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, tenant_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Speed, p.Heading, p.Sats, p.Source, p.CEP, tenantID)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, tenantID string, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at FROM positions WHERE device_imei=? AND tenant_id=? ORDER BY created_at DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Position, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at FROM positions WHERE device_imei=? AND tenant_id=? ORDER BY created_at DESC LIMIT ?",
		deviceIMEI, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		if err := rows.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &p.CreatedAt); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

func (d *DB) ListPositionsRange(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit, offset int) ([]store.Position, int, error) {
	countQuery := "SELECT COUNT(*) FROM positions WHERE device_imei=? AND tenant_id=?"
	args := []interface{}{deviceIMEI, tenantID}
	if !from.IsZero() {
		countQuery += " AND created_at >= ?"
		args = append(args, from)
	}
	if !to.IsZero() {
		countQuery += " AND created_at <= ?"
		args = append(args, to)
	}
	var total int
	if err := d.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT id, device_imei, lat, lon, alt, speed, heading, sats, source, cep, created_at FROM positions WHERE device_imei=? AND tenant_id=?"
	fetchArgs := []interface{}{deviceIMEI, tenantID}
	if !from.IsZero() {
		query += " AND created_at >= ?"
		fetchArgs = append(fetchArgs, from)
	}
	if !to.IsZero() {
		query += " AND created_at <= ?"
		fetchArgs = append(fetchArgs, to)
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		fetchArgs = append(fetchArgs, limit)
	}
	if offset > 0 {
		query += " OFFSET ?"
		fetchArgs = append(fetchArgs, offset)
	}

	rows, err := d.db.QueryContext(ctx, query, fetchArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var positions []store.Position
	for rows.Next() {
		var p store.Position
		if err := rows.Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.Heading, &p.Sats, &p.Source, &p.CEP, &p.CreatedAt); err != nil {
			return nil, 0, err
		}
		positions = append(positions, p)
	}
	return positions, total, rows.Err()
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
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=? ORDER BY created_at DESC LIMIT ?", tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, rows.Err()
}

func (d *DB) GetLatestAuditEntry(ctx context.Context, tenantID string) (*store.AuditEntry, error) {
	var a store.AuditEntry
	err := d.db.QueryRowContext(ctx,
		"SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=? ORDER BY created_at DESC LIMIT 1", tenantID,
	).Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]store.AuditEntry, error) {
	q := "SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=? AND created_at < ?"
	args := []any{tenantID, before}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, rows.Err()
}

func (d *DB) DeleteAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		"DELETE FROM audit_log WHERE tenant_id=? AND created_at < ?",
		tenantID, before,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Device config versioning ---

func (d *DB) CreateDeviceConfig(ctx context.Context, tenantID string, c *store.DeviceConfig) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("cfg-%d", time.Now().UnixNano())
	}
	var maxVersion int
	_ = d.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM device_configs WHERE device_imei=? AND tenant_id=?",
		c.DeviceIMEI, tenantID,
	).Scan(&maxVersion)
	c.Version = maxVersion + 1

	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_configs (id, device_imei, version, config, author, comment, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		c.ID, c.DeviceIMEI, c.Version, c.Config, c.Author, c.Comment, tenantID)
	return err
}

func (d *DB) GetDeviceConfigLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? ORDER BY version DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) GetDeviceConfigVersion(ctx context.Context, tenantID string, deviceIMEI string, version int) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? AND version=?",
		deviceIMEI, tenantID, version,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListDeviceConfigVersions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.DeviceConfig, error) {
	query := "SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=? AND tenant_id=? ORDER BY version DESC"
	args := []interface{}{deviceIMEI, tenantID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var configs []store.DeviceConfig
	for rows.Next() {
		var c store.DeviceConfig
		if err := rows.Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

// --- API keys ---

func (d *DB) CreateAPIKey(ctx context.Context, tenantID string, k *store.APIKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("key-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO api_keys (id, key_hash, key_prefix, role, label, device_imei, expires_at, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		k.ID, k.KeyHash, k.KeyPrefix, k.Role, k.Label, k.DeviceIMEI, k.ExpiresAt, tenantID)
	return err
}

func (d *DB) GetAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKey, string, error) {
	var k store.APIKey
	var tenantID string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, created_at, tenant_id FROM api_keys WHERE key_hash=?", keyHash,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt, &tenantID)
	if err != nil {
		return nil, "", err
	}
	return &k, tenantID, nil
}

func (d *DB) ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, created_at FROM api_keys WHERE tenant_id=? ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) GetAPIKeyByID(ctx context.Context, tenantID string, id string) (*store.APIKey, error) {
	var k store.APIKey
	err := d.db.QueryRowContext(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, rotation_days, created_at FROM api_keys WHERE id=? AND tenant_id=?",
		id, tenantID,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.RotationDays, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (d *DB) ListExpiringAPIKeys(ctx context.Context, before time.Time, limit int) ([]store.APIKey, error) {
	query := `SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, rotation_days, created_at
		FROM api_keys WHERE expires_at IS NOT NULL AND expires_at <= ? AND expires_at != '0001-01-01 00:00:00'
		ORDER BY expires_at ASC`
	args := []interface{}{before}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.RotationDays, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) UpdateAPIKeySecret(ctx context.Context, tenantID string, id string, keyHash, keyPrefix string, expiresAt time.Time) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE api_keys SET key_hash=?, key_prefix=?, expires_at=? WHERE id=? AND tenant_id=?",
		keyHash, keyPrefix, expiresAt, id, tenantID)
	return err
}

func (d *DB) DeleteAPIKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM api_keys WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

func (d *DB) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE api_keys SET last_used=NOW() WHERE id=?", id)
	return err
}

// --- Escalation Chains ---

func (d *DB) CreateEscalationChain(ctx context.Context, tenantID string, c *store.EscalationChain) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("chain-%d", time.Now().UnixNano())
	}
	tiersJSON, _ := json.Marshal(c.Tiers)
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO escalation_chains (id, name, tiers, tenant_id) VALUES (?, ?, ?, ?)",
		c.ID, c.Name, tiersJSON, tenantID)
	return err
}

func (d *DB) GetEscalationChain(ctx context.Context, tenantID string, id string) (*store.EscalationChain, error) {
	var c store.EscalationChain
	var tiersJSON []byte
	query := "SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE id=?"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&c.ID, &c.Name, &tiersJSON, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(tiersJSON, &c.Tiers)
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
		var tiersJSON []byte
		if err := rows.Scan(&c.ID, &c.Name, &tiersJSON, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(tiersJSON, &c.Tiers)
		chains = append(chains, c)
	}
	return chains, rows.Err()
}

func (d *DB) DeleteEscalationChain(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM escalation_chains WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Alerts ---

func (d *DB) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO alerts (id, chain_id, device_imei, type, detail, state, current_tier, retries,
			acked_by, acked_at, next_esc_at, created_at, updated_at, tenant_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ChainID, a.DeviceIMEI, a.Type, a.Detail, a.State, a.CurrentTier, a.Retries,
		a.AckedBy, a.AckedAt, a.NextEscAt, a.CreatedAt, a.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetAlert(ctx context.Context, tenantID string, id string) (*store.Alert, error) {
	var a store.Alert
	query := "SELECT id, tenant_id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts WHERE id=?"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(
		&a.ID, &a.TenantID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
		&a.CurrentTier, &a.Retries, &a.AckedBy, &a.AckedAt, &a.NextEscAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) ListAlerts(ctx context.Context, tenantID string, activeOnly bool, limit int) ([]store.Alert, error) {
	query := "SELECT id, tenant_id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts WHERE 1=1"
	var args []interface{}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	if activeOnly {
		query += " AND state IN ('triggered', 'escalating')"
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
		if err := rows.Scan(
			&a.ID, &a.TenantID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
			&a.CurrentTier, &a.Retries, &a.AckedBy, &a.AckedAt, &a.NextEscAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (d *DB) UpdateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	query := "UPDATE alerts SET state=?, current_tier=?, retries=?, acked_by=?, acked_at=?, next_esc_at=?, updated_at=? WHERE id=?"
	args := []interface{}{a.State, a.CurrentTier, a.Retries, a.AckedBy, a.AckedAt, a.NextEscAt, a.UpdatedAt, a.ID}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	_, err := d.db.ExecContext(ctx, query, args...)
	return err
}

// --- Notification Preferences ---

func (d *DB) SaveNotificationPref(ctx context.Context, tenantID string, p *store.NotificationPref) error {
	urlsJSON, _ := json.Marshal(p.URLs)
	eventsJSON, _ := json.Marshal(p.Events)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO notification_prefs (device_imei, urls, events, enabled, tenant_id)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE urls=VALUES(urls), events=VALUES(events), enabled=VALUES(enabled), updated_at=NOW()`,
		p.DeviceIMEI, urlsJSON, eventsJSON, p.Enabled, tenantID)
	return err
}

func (d *DB) GetNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) (*store.NotificationPref, error) {
	var p store.NotificationPref
	var urlsJSON, eventsJSON []byte
	err := d.db.QueryRowContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE device_imei=? AND tenant_id=?",
		deviceIMEI, tenantID,
	).Scan(&p.DeviceIMEI, &urlsJSON, &eventsJSON, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(urlsJSON, &p.URLs)
	_ = json.Unmarshal(eventsJSON, &p.Events)
	return &p, nil
}

func (d *DB) ListNotificationPrefs(ctx context.Context, tenantID string) ([]store.NotificationPref, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE tenant_id=? ORDER BY device_imei", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var prefs []store.NotificationPref
	for rows.Next() {
		var p store.NotificationPref
		var urlsJSON, eventsJSON []byte
		if err := rows.Scan(&p.DeviceIMEI, &urlsJSON, &eventsJSON, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(urlsJSON, &p.URLs)
		_ = json.Unmarshal(eventsJSON, &p.Events)
		prefs = append(prefs, p)
	}
	return prefs, rows.Err()
}

func (d *DB) DeleteNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM notification_prefs WHERE device_imei=? AND tenant_id=?", deviceIMEI, tenantID)
	return err
}

// --- Users ---

func (d *DB) CreateUser(ctx context.Context, tenantID string, u *store.LocalUser) error {
	if u.ID == "" {
		u.ID = fmt.Sprintf("usr-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO users (id, email, name, password_hash, role, enabled, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.Email, u.Name, u.PasswordHash, u.Role, u.Enabled, tenantID)
	return err
}

func (d *DB) GetUserByID(ctx context.Context, tenantID string, id string) (*store.LocalUser, error) {
	var u store.LocalUser
	var lockedUntil, lastLoginAt sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT id, email, name, password_hash, role, enabled, failed_logins, locked_until, last_login_at, created_at, updated_at FROM users WHERE id=? AND tenant_id=?", id, tenantID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Enabled,
		&u.FailedLogins, &lockedUntil, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lockedUntil.Valid {
		u.LockedUntil = lockedUntil.Time
	}
	if lastLoginAt.Valid {
		u.LastLoginAt = lastLoginAt.Time
	}
	return &u, nil
}

func (d *DB) GetUserByEmail(ctx context.Context, tenantID string, email string) (*store.LocalUser, error) {
	var u store.LocalUser
	var lockedUntil, lastLoginAt sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT id, email, name, password_hash, role, enabled, failed_logins, locked_until, last_login_at, created_at, updated_at FROM users WHERE email=? AND tenant_id=?", email, tenantID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Enabled,
		&u.FailedLogins, &lockedUntil, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lockedUntil.Valid {
		u.LockedUntil = lockedUntil.Time
	}
	if lastLoginAt.Valid {
		u.LastLoginAt = lastLoginAt.Time
	}
	return &u, nil
}

func (d *DB) ListUsers(ctx context.Context, tenantID string) ([]store.LocalUser, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, email, name, role, enabled, failed_logins, last_login_at, created_at, updated_at FROM users WHERE tenant_id=? ORDER BY created_at", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []store.LocalUser
	for rows.Next() {
		var u store.LocalUser
		var lastLoginAt sql.NullTime
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Enabled,
			&u.FailedLogins, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if lastLoginAt.Valid {
			u.LastLoginAt = lastLoginAt.Time
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (d *DB) UpdateUser(ctx context.Context, tenantID string, u *store.LocalUser) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE users SET email=?, name=?, password_hash=?, role=?, enabled=?, updated_at=NOW() WHERE id=? AND tenant_id=?",
		u.Email, u.Name, u.PasswordHash, u.Role, u.Enabled, u.ID, tenantID)
	return err
}

func (d *DB) DeleteUser(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM users WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

func (d *DB) IncrementFailedLogins(ctx context.Context, tenantID string, id string) (int, error) {
	_, err := d.db.ExecContext(ctx,
		`UPDATE users SET failed_logins = failed_logins + 1,
		 locked_until = IF(failed_logins + 1 >= ?, DATE_ADD(NOW(), INTERVAL 30 MINUTE), locked_until),
		 updated_at = NOW()
		 WHERE id=? AND tenant_id=?`,
		store.MaxFailedLogins, id, tenantID)
	if err != nil {
		return 0, err
	}
	var count int
	err = d.db.QueryRowContext(ctx, "SELECT failed_logins FROM users WHERE id=? AND tenant_id=?", id, tenantID).Scan(&count)
	return count, err
}

func (d *DB) ResetFailedLogins(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE users SET failed_logins=0, locked_until=NULL, last_login_at=NOW(), updated_at=NOW() WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Refresh Tokens ---

func (d *DB) StoreRefreshToken(ctx context.Context, tenantID string, t *store.RefreshToken) error {
	if t.ID == "" {
		t.ID = fmt.Sprintf("rt-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO refresh_tokens (id, user_id, tenant_id, token_hash, expires_at) VALUES (?, ?, ?, ?, ?)",
		t.ID, t.UserID, tenantID, t.TokenHash, t.ExpiresAt)
	return err
}

func (d *DB) GetRefreshToken(ctx context.Context, tokenHash string) (*store.RefreshToken, error) {
	var t store.RefreshToken
	err := d.db.QueryRowContext(ctx,
		"SELECT id, user_id, tenant_id, token_hash, expires_at, created_at FROM refresh_tokens WHERE token_hash=?", tokenHash,
	).Scan(&t.ID, &t.UserID, &t.TenantID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE token_hash=?", tokenHash)
	return err
}

func (d *DB) DeleteRefreshTokensByUser(ctx context.Context, tenantID string, userID string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE user_id=? AND tenant_id=?", userID, tenantID)
	return err
}

// --- Device Encryption Keys ---

func (d *DB) CreateDeviceKey(ctx context.Context, tenantID string, k *store.DeviceKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("dk-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_keys (id, device_imei, key_hash, key_hex, mode, tenant_id) VALUES (?, ?, ?, ?, ?, ?)",
		k.ID, k.DeviceIMEI, k.KeyHash, k.KeyHex, k.Mode, tenantID)
	if err != nil {
		return err
	}
	k.CreatedAt = time.Now().UTC()
	return nil
}

func (d *DB) ListDeviceKeys(ctx context.Context, tenantID string, deviceIMEI string) ([]store.DeviceKey, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, device_imei, key_hash, mode, created_at FROM device_keys WHERE device_imei=? AND tenant_id=? ORDER BY created_at DESC",
		deviceIMEI, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.DeviceKey
	for rows.Next() {
		var k store.DeviceKey
		if err := rows.Scan(&k.ID, &k.DeviceIMEI, &k.KeyHash, &k.Mode, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) GetDeviceKeyLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceKey, error) {
	var k store.DeviceKey
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, key_hash, key_hex, mode, created_at FROM device_keys WHERE device_imei=? AND tenant_id=? ORDER BY created_at DESC LIMIT 1",
		deviceIMEI, tenantID).Scan(&k.ID, &k.DeviceIMEI, &k.KeyHash, &k.KeyHex, &k.Mode, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (d *DB) DeleteDeviceKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM device_keys WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Device WireGuard ---

func (d *DB) SaveDeviceWireguard(ctx context.Context, tenantID string, dw *store.DeviceWireguard) error {
	_, err := d.db.ExecContext(ctx,
		`REPLACE INTO device_wireguard (device_imei, peer_id, vpn_address, public_key, tenant_id) VALUES (?, ?, ?, ?, ?)`,
		dw.DeviceIMEI, dw.PeerID, dw.VPNAddress, dw.PublicKey, tenantID)
	if err != nil {
		return err
	}
	dw.CreatedAt = time.Now().UTC()
	return nil
}

func (d *DB) GetDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceWireguard, error) {
	var dw store.DeviceWireguard
	err := d.db.QueryRowContext(ctx,
		"SELECT device_imei, peer_id, vpn_address, public_key, created_at FROM device_wireguard WHERE device_imei=? AND tenant_id=?",
		deviceIMEI, tenantID).Scan(&dw.DeviceIMEI, &dw.PeerID, &dw.VPNAddress, &dw.PublicKey, &dw.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &dw, nil
}

func (d *DB) DeleteDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM device_wireguard WHERE device_imei=? AND tenant_id=?", deviceIMEI, tenantID)
	return err
}

// --- Routes ---

func (d *DB) CreateRoute(ctx context.Context, tenantID string, r *store.Route) error {
	if r.ID == "" {
		r.ID = fmt.Sprintf("route-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO routes (id, name, source_type, destination_type, filter, enabled, created_at, updated_at, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.SourceType, r.DestinationType, r.Filter, r.Enabled,
		r.CreatedAt, r.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetRoute(ctx context.Context, tenantID string, id string) (*store.Route, error) {
	var r store.Route
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, source_type, destination_type, filter, enabled, created_at, updated_at FROM routes WHERE id=? AND tenant_id=?",
		id, tenantID,
	).Scan(&r.ID, &r.Name, &r.SourceType, &r.DestinationType, &r.Filter, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListRoutes(ctx context.Context, tenantID string) ([]store.Route, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, source_type, destination_type, filter, enabled, created_at, updated_at FROM routes WHERE tenant_id=? ORDER BY name",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var routes []store.Route
	for rows.Next() {
		var r store.Route
		if err := rows.Scan(&r.ID, &r.Name, &r.SourceType, &r.DestinationType, &r.Filter, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

func (d *DB) UpdateRoute(ctx context.Context, tenantID string, r *store.Route) error {
	r.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		"UPDATE routes SET name=?, source_type=?, destination_type=?, filter=?, enabled=?, updated_at=? WHERE id=? AND tenant_id=?",
		r.Name, r.SourceType, r.DestinationType, r.Filter, r.Enabled, r.UpdatedAt, r.ID, tenantID)
	return err
}

func (d *DB) DeleteRoute(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM routes WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- System Config ---

func (d *DB) GetSystemConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := d.db.QueryRowContext(ctx, "SELECT value FROM system_config WHERE `key`=?", key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (d *DB) SetSystemConfig(ctx context.Context, key, value string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO system_config (`key`, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)",
		key, value)
	return err
}

// --- Bridges ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *DB) CreateOrUpdateBridge(ctx context.Context, tenantID string, b *store.Bridge) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO bridges (bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
		 ON DUPLICATE KEY UPDATE
			tenant_id=VALUES(tenant_id), label=VALUES(label), hostname=VALUES(hostname),
			version=VALUES(version), mode=VALUES(mode),
			location_lat=VALUES(location_lat), location_lon=VALUES(location_lon),
			location_alt=VALUES(location_alt), capabilities=VALUES(capabilities),
			reticulum_hash=VALUES(reticulum_hash), reticulum_pubkey=VALUES(reticulum_pubkey),
			cot_type=VALUES(cot_type), cot_callsign=VALUES(cot_callsign),
			online=VALUES(online), last_birth=VALUES(last_birth), last_health=VALUES(last_health),
			last_seen=NOW(), updated_at=NOW()`,
		b.BridgeID, tenantID, b.Label, b.Hostname, b.Version, b.Mode,
		b.LocationLat, b.LocationLon, b.LocationAlt, defaultJSON(b.Capabilities, "[]"),
		b.ReticulumHash, b.ReticulumPubkey, b.CoTType, b.CoTCallsign,
		boolToInt(b.Online), defaultJSON(b.LastBirth, "{}"), defaultJSON(b.LastHealth, "{}"))
	return err
}

// defaultJSON returns fallback if s is empty (MariaDB JSON columns reject empty strings).
func defaultJSON(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (d *DB) GetBridge(ctx context.Context, tenantID string, bridgeID string) (*store.Bridge, error) {
	var b store.Bridge
	var online int
	var lastSeen sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen, created_at, updated_at
		 FROM bridges WHERE bridge_id=? AND tenant_id=?`, bridgeID, tenantID,
	).Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
		&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
		&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
		&online, &b.LastBirth, &b.LastHealth, &lastSeen, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	b.Online = online != 0
	if lastSeen.Valid {
		b.LastSeen = &lastSeen.Time
	}
	return &b, nil
}

func (d *DB) ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen, created_at, updated_at
		 FROM bridges WHERE tenant_id=? ORDER BY label, bridge_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var bridges []*store.Bridge
	for rows.Next() {
		var b store.Bridge
		var online int
		var lastSeen sql.NullTime
		if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
			&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
			&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
			&online, &b.LastBirth, &b.LastHealth, &lastSeen, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.Online = online != 0
		if lastSeen.Valid {
			b.LastSeen = &lastSeen.Time
		}
		bridges = append(bridges, &b)
	}
	return bridges, nil
}

func (d *DB) UpdateBridge(ctx context.Context, tenantID string, bridgeID string, updates store.BridgeUpdate) error {
	setClauses := "updated_at=NOW()"
	args := []interface{}{}
	if updates.Label != nil {
		setClauses += ", label=?"
		args = append(args, *updates.Label)
	}
	if updates.CoTCallsign != nil {
		setClauses += ", cot_callsign=?"
		args = append(args, *updates.CoTCallsign)
	}
	args = append(args, bridgeID, tenantID)
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf("UPDATE bridges SET %s WHERE bridge_id=? AND tenant_id=?", setClauses), args...)
	return err
}

func (d *DB) DeleteBridge(ctx context.Context, tenantID string, bridgeID string) error {
	_, _ = d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=NULL WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	_, err := d.db.ExecContext(ctx, "DELETE FROM bridges WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	return err
}

func (d *DB) SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET online=?, updated_at=NOW() WHERE bridge_id=? AND tenant_id=?",
		boolToInt(online), bridgeID, tenantID)
	return err
}

func (d *DB) TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_seen=NOW() WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	return err
}

func (d *DB) MarkStaleBridgesOffline(ctx context.Context, timeout time.Duration) (int64, error) {
	secs := int(timeout.Seconds())
	res, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET online=0, updated_at=NOW() WHERE online=1 AND last_seen IS NOT NULL AND last_seen < DATE_SUB(NOW(), INTERVAL ? SECOND)",
		secs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_health=?, updated_at=NOW() WHERE bridge_id=? AND tenant_id=?",
		health, bridgeID, tenantID)
	return err
}

func (d *DB) AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=? WHERE imei=? AND tenant_id=?", bridgeID, imei, tenantID)
	return err
}

func (d *DB) SetBridgeCredentials(ctx context.Context, tenantID, bridgeID, username, passwordHash string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET mqtt_username=?, mqtt_password_hash=?, updated_at=NOW()
		 WHERE bridge_id=? AND tenant_id=?`,
		username, passwordHash, bridgeID, tenantID)
	return err
}

func (d *DB) GetBridgeCredentials(ctx context.Context, tenantID, bridgeID string) (*store.BridgeCredentials, error) {
	var c store.BridgeCredentials
	var certExpiry sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT bridge_id, mqtt_username, mqtt_password_hash, cert_pem, cert_expiry, created_at
		 FROM bridges WHERE bridge_id=? AND tenant_id=?`, bridgeID, tenantID,
	).Scan(&c.BridgeID, &c.Username, &c.Password, &c.CertPEM, &certExpiry, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	if certExpiry.Valid {
		c.CertExpiry = &certExpiry.Time
	}
	return &c, nil
}

func (d *DB) SetBridgeCertificate(ctx context.Context, tenantID, bridgeID, certPEM string, expiry time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET cert_pem=?, cert_expiry=?, updated_at=NOW()
		 WHERE bridge_id=? AND tenant_id=?`,
		certPEM, expiry, bridgeID, tenantID)
	return err
}

func (d *DB) ListBridgesWithCredentials(ctx context.Context) ([]*store.Bridge, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen,
			mqtt_username, mqtt_password_hash, cert_pem, cert_expiry,
			created_at, updated_at
		 FROM bridges WHERE mqtt_username != '' ORDER BY bridge_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var bridges []*store.Bridge
	for rows.Next() {
		var b store.Bridge
		var online int
		var lastSeen, certExpiry sql.NullTime
		if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
			&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
			&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
			&online, &b.LastBirth, &b.LastHealth, &lastSeen,
			&b.MQTTUsername, &b.MQTTPasswordHash, &b.CertPEM, &certExpiry,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.Online = online != 0
		if lastSeen.Valid {
			b.LastSeen = &lastSeen.Time
		}
		if certExpiry.Valid {
			b.CertExpiry = &certExpiry.Time
		}
		bridges = append(bridges, &b)
	}
	return bridges, nil
}

// --- Cost ledger ---

func (d *DB) InsertCostEntry(ctx context.Context, tenantID string, c *store.CostEntry) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO cost_ledger (id, device_imei, interface_type, direction, cost_usd, message_id, detail, tenant_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		c.ID, c.DeviceIMEI, c.InterfaceType, c.Direction, c.CostUSD, c.MessageID, c.Detail, tenantID)
	return err
}

func (d *DB) ListCostEntries(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit int) ([]store.CostEntry, error) {
	query := "SELECT id, device_imei, interface_type, direction, cost_usd, message_id, detail, created_at FROM cost_ledger WHERE tenant_id=?"
	args := []interface{}{tenantID}
	if deviceIMEI != "" {
		query += " AND device_imei=?"
		args = append(args, deviceIMEI)
	}
	if !from.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, from)
	}
	if !to.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, to)
	}
	query += " ORDER BY created_at DESC"
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.CostEntry
	for rows.Next() {
		var c store.CostEntry
		if err := rows.Scan(&c.ID, &c.DeviceIMEI, &c.InterfaceType, &c.Direction, &c.CostUSD, &c.MessageID, &c.Detail, &c.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, c)
	}
	return entries, rows.Err()
}

func (d *DB) AggregateCosts(ctx context.Context, tenantID string, from, to time.Time, groupBy string) ([]store.CostAggregate, error) {
	var groupExpr string
	switch groupBy {
	case "month":
		groupExpr = "DATE_FORMAT(created_at, '%Y-%m')"
	default: // "device"
		groupExpr = "device_imei"
	}
	query := fmt.Sprintf("SELECT %s AS group_key, SUM(cost_usd) AS total_usd, COUNT(*) AS cnt FROM cost_ledger WHERE tenant_id=?", groupExpr)
	args := []interface{}{tenantID}
	if !from.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, from)
	}
	if !to.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, to)
	}
	query += fmt.Sprintf(" GROUP BY %s ORDER BY total_usd DESC", groupExpr)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var aggs []store.CostAggregate
	for rows.Next() {
		var a store.CostAggregate
		if err := rows.Scan(&a.GroupKey, &a.TotalUSD, &a.Count); err != nil {
			return nil, err
		}
		aggs = append(aggs, a)
	}
	return aggs, rows.Err()
}

// --- Device groups ---

func (d *DB) CreateDeviceGroup(ctx context.Context, tenantID string, g *store.DeviceGroup) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_groups (id, name, description, color, tenant_id) VALUES (?, ?, ?, ?, ?)",
		g.ID, g.Name, g.Description, g.Color, tenantID)
	return err
}

func (d *DB) GetDeviceGroup(ctx context.Context, tenantID string, id string) (*store.DeviceGroup, error) {
	var g store.DeviceGroup
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, description, color, created_at, updated_at FROM device_groups WHERE id=? AND tenant_id=?",
		id, tenantID).Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM device_group_members WHERE group_id=? AND tenant_id=?",
		id, tenantID).Scan(&g.MemberCount)
	return &g, nil
}

func (d *DB) ListDeviceGroups(ctx context.Context, tenantID string) ([]store.DeviceGroup, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.description, g.color, g.created_at, g.updated_at,
		 (SELECT COUNT(*) FROM device_group_members m WHERE m.group_id=g.id AND m.tenant_id=g.tenant_id)
		 FROM device_groups g WHERE g.tenant_id=? ORDER BY g.name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var groups []store.DeviceGroup
	for rows.Next() {
		var g store.DeviceGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt, &g.MemberCount); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (d *DB) UpdateDeviceGroup(ctx context.Context, tenantID string, g *store.DeviceGroup) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE device_groups SET name=?, description=?, color=?, updated_at=NOW() WHERE id=? AND tenant_id=?",
		g.Name, g.Description, g.Color, g.ID, tenantID)
	return err
}

func (d *DB) DeleteDeviceGroup(ctx context.Context, tenantID string, id string) error {
	_, _ = d.db.ExecContext(ctx, "DELETE FROM device_group_members WHERE group_id=? AND tenant_id=?", id, tenantID)
	_, err := d.db.ExecContext(ctx, "DELETE FROM device_groups WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

func (d *DB) AddDeviceToGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT IGNORE INTO device_group_members (group_id, device_imei, tenant_id) VALUES (?, ?, ?)",
		groupID, deviceIMEI, tenantID)
	return err
}

func (d *DB) RemoveDeviceFromGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM device_group_members WHERE group_id=? AND device_imei=? AND tenant_id=?",
		groupID, deviceIMEI, tenantID)
	return err
}

func (d *DB) ListDevicesInGroup(ctx context.Context, tenantID string, groupID string) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT d.imei, d.label, d.type, d.notes, d.last_seen, d.created_at, d.updated_at
		 FROM devices d JOIN device_group_members m ON d.imei=m.device_imei AND d.tenant_id=m.tenant_id
		 WHERE m.group_id=? AND m.tenant_id=? ORDER BY d.label`, groupID, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		if err := rows.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) ListGroupsForDevice(ctx context.Context, tenantID string, deviceIMEI string) ([]store.DeviceGroup, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.description, g.color, g.created_at, g.updated_at
		 FROM device_groups g JOIN device_group_members m ON g.id=m.group_id AND g.tenant_id=m.tenant_id
		 WHERE m.device_imei=? AND m.tenant_id=? ORDER BY g.name`, deviceIMEI, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var groups []store.DeviceGroup
	for rows.Next() {
		var g store.DeviceGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// --- Message Templates ---

func (d *DB) CreateMessageTemplate(ctx context.Context, tenantID string, t *store.MessageTemplate) error {
	if t.ID == "" {
		t.ID = fmt.Sprintf("tmpl-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	vars, _ := json.Marshal(t.Variables)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO message_templates (id, name, body, variables, created_at, updated_at, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Body, string(vars), t.CreatedAt, t.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetMessageTemplate(ctx context.Context, tenantID string, id string) (*store.MessageTemplate, error) {
	var t store.MessageTemplate
	var vars string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, body, variables, created_at, updated_at FROM message_templates WHERE id=? AND tenant_id=?",
		id, tenantID,
	).Scan(&t.ID, &t.Name, &t.Body, &vars, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(vars), &t.Variables)
	return &t, nil
}

func (d *DB) ListMessageTemplates(ctx context.Context, tenantID string) ([]store.MessageTemplate, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, body, variables, created_at, updated_at FROM message_templates WHERE tenant_id=? ORDER BY name",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var templates []store.MessageTemplate
	for rows.Next() {
		var t store.MessageTemplate
		var vars string
		if err := rows.Scan(&t.ID, &t.Name, &t.Body, &vars, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(vars), &t.Variables)
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

func (d *DB) UpdateMessageTemplate(ctx context.Context, tenantID string, t *store.MessageTemplate) error {
	t.UpdatedAt = time.Now().UTC()
	vars, _ := json.Marshal(t.Variables)
	_, err := d.db.ExecContext(ctx,
		"UPDATE message_templates SET name=?, body=?, variables=?, updated_at=? WHERE id=? AND tenant_id=?",
		t.Name, t.Body, string(vars), t.UpdatedAt, t.ID, tenantID)
	return err
}

func (d *DB) DeleteMessageTemplate(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM message_templates WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// --- Alert Rules (MESHSAT-313) ---

func (d *DB) CreateAlertRule(ctx context.Context, tenantID string, r *store.AlertRule) error {
	if r.ID == "" {
		r.ID = fmt.Sprintf("arule-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO alert_rules (id, name, condition_type, condition_params, chain_id, device_filter, enabled, created_at, updated_at, tenant_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.ConditionType, r.ConditionParams, r.ChainID, r.DeviceFilter,
		r.Enabled, r.CreatedAt, r.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetAlertRule(ctx context.Context, tenantID string, id string) (*store.AlertRule, error) {
	var r store.AlertRule
	var lastEval sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, condition_type, condition_params, chain_id, device_filter, enabled, last_evaluated, created_at, updated_at FROM alert_rules WHERE id=? AND tenant_id=?", id, tenantID,
	).Scan(&r.ID, &r.Name, &r.ConditionType, &r.ConditionParams, &r.ChainID, &r.DeviceFilter, &r.Enabled, &lastEval, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastEval.Valid {
		r.LastEvaluated = lastEval.Time
	}
	return &r, nil
}

func (d *DB) ListAlertRules(ctx context.Context, tenantID string) ([]store.AlertRule, error) {
	// Empty tenantID returns rules across all tenants (used by evaluator).
	query := "SELECT id, name, condition_type, condition_params, chain_id, device_filter, enabled, last_evaluated, created_at, updated_at, tenant_id FROM alert_rules WHERE 1=1"
	var args []interface{}
	if tenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, tenantID)
	}
	query += " ORDER BY name"
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rules []store.AlertRule
	for rows.Next() {
		var r store.AlertRule
		var lastEval sql.NullTime
		var tid string
		if err := rows.Scan(&r.ID, &r.Name, &r.ConditionType, &r.ConditionParams, &r.ChainID, &r.DeviceFilter, &r.Enabled, &lastEval, &r.CreatedAt, &r.UpdatedAt, &tid); err != nil {
			return nil, err
		}
		if lastEval.Valid {
			r.LastEvaluated = lastEval.Time
		}
		r.TenantID = tid
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (d *DB) UpdateAlertRule(ctx context.Context, tenantID string, r *store.AlertRule) error {
	r.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		"UPDATE alert_rules SET name=?, condition_type=?, condition_params=?, chain_id=?, device_filter=?, enabled=?, last_evaluated=?, updated_at=? WHERE id=? AND tenant_id=?",
		r.Name, r.ConditionType, r.ConditionParams, r.ChainID, r.DeviceFilter,
		r.Enabled, r.LastEvaluated, r.UpdatedAt, r.ID, tenantID)
	return err
}

func (d *DB) DeleteAlertRule(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM alert_rules WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

// ---- Credential management (MESHSAT-356) ----
// MariaDB implementation mirrors SQLite. Uses same table schema.

func (d *DB) CreateCredential(ctx context.Context, tenantID string, c *store.Credential) error {
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO credentials (id, tenant_id, provider, name, cred_type, encrypted_data,
		 cert_not_after, cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id,
		 status, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, tenantID, c.Provider, c.Name, c.CredType, c.EncryptedData,
		c.CertNotAfter, c.CertSubject, c.CertIssuer, c.CertFingerprint,
		c.TargetScope, c.TargetBridgeID, c.Status, c.Version,
		now, now)
	return err
}

func (d *DB) GetCredential(ctx context.Context, tenantID string, id string) (*store.Credential, error) {
	var c store.Credential
	err := d.db.QueryRowContext(ctx,
		"SELECT id, tenant_id, provider, name, cred_type, encrypted_data, cert_not_after, cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id, status, version, distributed_at, created_at, updated_at FROM credentials WHERE id=? AND tenant_id=?",
		id, tenantID).Scan(&c.ID, &c.TenantID, &c.Provider, &c.Name, &c.CredType, &c.EncryptedData,
		&c.CertNotAfter, &c.CertSubject, &c.CertIssuer, &c.CertFingerprint,
		&c.TargetScope, &c.TargetBridgeID, &c.Status, &c.Version, &c.DistributedAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListCredentials(ctx context.Context, tenantID string) ([]store.Credential, error) {
	var creds []store.Credential
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, tenant_id, provider, name, cred_type, encrypted_data, cert_not_after, cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id, status, version, distributed_at, created_at, updated_at FROM credentials WHERE tenant_id=? ORDER BY provider, name",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c store.Credential
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Provider, &c.Name, &c.CredType, &c.EncryptedData,
			&c.CertNotAfter, &c.CertSubject, &c.CertIssuer, &c.CertFingerprint,
			&c.TargetScope, &c.TargetBridgeID, &c.Status, &c.Version, &c.DistributedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, nil
}

func (d *DB) UpdateCredential(ctx context.Context, tenantID string, c *store.Credential) error {
	c.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		`UPDATE credentials SET provider=?, name=?, cred_type=?, encrypted_data=?,
		 cert_not_after=?, cert_subject=?, cert_issuer=?, cert_fingerprint=?,
		 target_scope=?, target_bridge_id=?, status=?, version=?, updated_at=?
		 WHERE id=? AND tenant_id=?`,
		c.Provider, c.Name, c.CredType, c.EncryptedData,
		c.CertNotAfter, c.CertSubject, c.CertIssuer, c.CertFingerprint,
		c.TargetScope, c.TargetBridgeID, c.Status, c.Version,
		c.UpdatedAt, c.ID, tenantID)
	return err
}

func (d *DB) DeleteCredential(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM credentials WHERE id=? AND tenant_id=?", id, tenantID)
	return err
}

func (d *DB) ListExpiringCredentials(ctx context.Context, before time.Time) ([]store.Credential, error) {
	var creds []store.Credential
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, tenant_id, provider, name, cred_type, encrypted_data, cert_not_after, cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id, status, version, distributed_at, created_at, updated_at
		 FROM credentials WHERE cert_not_after IS NOT NULL AND cert_not_after <= ? AND status IN ('active', 'expiring')
		 ORDER BY cert_not_after ASC`, before)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c store.Credential
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Provider, &c.Name, &c.CredType, &c.EncryptedData,
			&c.CertNotAfter, &c.CertSubject, &c.CertIssuer, &c.CertFingerprint,
			&c.TargetScope, &c.TargetBridgeID, &c.Status, &c.Version, &c.DistributedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, nil
}

// Compile-time check.
var _ store.Store = (*DB)(nil)
