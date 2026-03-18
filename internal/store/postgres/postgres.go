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
	// Tenant isolation: add tenant_id to all tables (PostgreSQL supports IF NOT EXISTS).
	`ALTER TABLE devices ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE messages ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE webhook_configs ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE delivery_logs ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE positions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	`ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS prev_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS hash TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_devices_tenant ON devices(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_tenant ON messages(tenant_id, device_imei)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_configs_tenant ON webhook_configs(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_positions_tenant ON positions(tenant_id, device_imei)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_log_tenant ON audit_log(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_logs_tenant ON delivery_logs(tenant_id)`,
	// API keys
	`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		key_hash TEXT NOT NULL UNIQUE,
		key_prefix TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT 'viewer',
		label TEXT NOT NULL DEFAULT '',
		device_imei TEXT NOT NULL DEFAULT '',
		last_used TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01',
		expires_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tenant_id TEXT NOT NULL DEFAULT 'default'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id)`,
	// Device config versioning
	`CREATE TABLE IF NOT EXISTS device_configs (
		id TEXT PRIMARY KEY,
		device_imei TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1,
		config TEXT NOT NULL DEFAULT '{}',
		author TEXT NOT NULL DEFAULT '',
		comment TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tenant_id TEXT NOT NULL DEFAULT 'default',
		UNIQUE(device_imei, version, tenant_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_configs_device ON device_configs(device_imei, tenant_id, version DESC)`,
	// Escalation chains (v0.3)
	`CREATE TABLE IF NOT EXISTS escalation_chains (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		tiers JSONB NOT NULL DEFAULT '[]',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
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
		acked_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01',
		next_esc_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tenant_id TEXT NOT NULL DEFAULT 'default'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_alerts_tenant ON alerts(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_alerts_state ON alerts(state, next_esc_at)`,
}

// --- Devices ---

func (d *DB) CreateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.pool.Exec(ctx,
		"INSERT INTO devices (imei, label, type, notes, tenant_id) VALUES ($1, $2, $3, $4, $5)",
		dev.IMEI, dev.Label, dev.Type, dev.Notes, tenantID)
	return err
}

func (d *DB) GetDevice(ctx context.Context, tenantID string, imei string) (*store.Device, error) {
	var dev store.Device
	err := d.pool.QueryRow(ctx,
		"SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID,
	).Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

func (d *DB) ListDevices(ctx context.Context, tenantID string) ([]store.Device, error) {
	rows, err := d.pool.Query(ctx, "SELECT imei, label, type, notes, last_seen, created_at, updated_at FROM devices WHERE tenant_id=$1 ORDER BY label, imei", tenantID)
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

func (d *DB) UpdateDevice(ctx context.Context, tenantID string, dev *store.Device) error {
	_, err := d.pool.Exec(ctx,
		"UPDATE devices SET label=$1, type=$2, notes=$3, updated_at=NOW() WHERE imei=$4 AND tenant_id=$5",
		dev.Label, dev.Type, dev.Notes, dev.IMEI, tenantID)
	return err
}

func (d *DB) DeleteDevice(ctx context.Context, tenantID string, imei string) error {
	_, err := d.pool.Exec(ctx, "DELETE FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID)
	return err
}

func (d *DB) TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error {
	_, err := d.pool.Exec(ctx, "UPDATE devices SET last_seen=NOW() WHERE imei=$1 AND tenant_id=$2", imei, tenantID)
	return err
}

// --- Messages ---

func (d *DB) InsertMessage(ctx context.Context, tenantID string, m *store.Message) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO messages (id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		m.ID, m.DeviceIMEI, m.Direction, m.Channel, m.MOMSN, m.Text, m.RawHex,
		m.Compressed, m.Status, m.Error, m.Lat, m.Lon, tenantID)
	return err
}

func (d *DB) ListMessages(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Message, error) {
	query := "SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE tenant_id=$1"
	args := []interface{}{tenantID}
	argN := 2
	if deviceIMEI != "" {
		query += fmt.Sprintf(" AND device_imei=$%d", argN)
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

func (d *DB) GetMessage(ctx context.Context, tenantID string, id string) (*store.Message, error) {
	var m store.Message
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, direction, channel, momsn, text, raw_hex, compressed, status, error, lat, lon, created_at FROM messages WHERE id=$1 AND tenant_id=$2", id, tenantID,
	).Scan(&m.ID, &m.DeviceIMEI, &m.Direction, &m.Channel, &m.MOMSN, &m.Text, &m.RawHex,
		&m.Compressed, &m.Status, &m.Error, &m.Lat, &m.Lon, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// --- Webhooks ---

func (d *DB) SaveWebhook(ctx context.Context, tenantID string, w *store.WebhookConfig) error {
	eventsJSON, _ := json.Marshal(w.Events)
	_, err := d.pool.Exec(ctx,
		`INSERT INTO webhook_configs (id, url, secret, events, max_retries, timeout_sec, enabled, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE SET url=$2, secret=$3, events=$4, max_retries=$5, timeout_sec=$6, enabled=$7`,
		w.ID, w.URL, w.Secret, eventsJSON, w.MaxRetries, w.TimeoutSec, w.Enabled, tenantID)
	return err
}

func (d *DB) ListWebhooks(ctx context.Context, tenantID string) ([]store.WebhookConfig, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, url, secret, events, max_retries, timeout_sec, enabled, created_at FROM webhook_configs WHERE tenant_id=$1", tenantID)
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

func (d *DB) DeleteWebhook(ctx context.Context, tenantID string, id string) error {
	_, err := d.pool.Exec(ctx, "DELETE FROM webhook_configs WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// --- Delivery logs ---

func (d *DB) InsertDeliveryLog(ctx context.Context, tenantID string, l *store.DeliveryLog) error {
	if l.ID == "" {
		l.ID = fmt.Sprintf("dl-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO delivery_logs (id, webhook_id, event, device_imei, status_code, error, attempt, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		l.ID, l.WebhookID, l.Event, l.DeviceIMEI, l.StatusCode, l.Error, l.Attempt, tenantID)
	return err
}

func (d *DB) ListDeliveryLogs(ctx context.Context, tenantID string, limit int) ([]store.DeliveryLog, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, webhook_id, event, device_imei, status_code, error, attempt, created_at FROM delivery_logs WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2", tenantID, limit)
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

func (d *DB) InsertPosition(ctx context.Context, tenantID string, p *store.Position) error {
	if p.ID == "" {
		p.ID = fmt.Sprintf("pos-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO positions (id, device_imei, lat, lon, alt, source, cep, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.ID, p.DeviceIMEI, p.Lat, p.Lon, p.Alt, p.Source, p.CEP, tenantID)
	return err
}

func (d *DB) LatestPosition(ctx context.Context, tenantID string, deviceIMEI string) (*store.Position, error) {
	var p store.Position
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&p.ID, &p.DeviceIMEI, &p.Lat, &p.Lon, &p.Alt, &p.Source, &p.CEP, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ListPositions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.Position, error) {
	rows, err := d.pool.Query(ctx,
		"SELECT id, device_imei, lat, lon, alt, source, cep, created_at FROM positions WHERE device_imei=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT $3",
		deviceIMEI, tenantID, limit)
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

func (d *DB) InsertAuditEntry(ctx context.Context, tenantID string, a *store.AuditEntry) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("aud-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		"INSERT INTO audit_log (id, action, actor, detail, ip, prev_hash, hash, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		a.ID, a.Action, a.Actor, a.Detail, a.IP, a.PrevHash, a.Hash, tenantID)
	return err
}

func (d *DB) ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]store.AuditEntry, error) {
	rows, err := d.pool.Query(ctx, "SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2", tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []store.AuditEntry
	for rows.Next() {
		var a store.AuditEntry
		if err := rows.Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, a)
	}
	return entries, nil
}

func (d *DB) GetLatestAuditEntry(ctx context.Context, tenantID string) (*store.AuditEntry, error) {
	var a store.AuditEntry
	err := d.pool.QueryRow(ctx,
		"SELECT id, action, actor, detail, ip, prev_hash, hash, created_at FROM audit_log WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1",
		tenantID,
	).Scan(&a.ID, &a.Action, &a.Actor, &a.Detail, &a.IP, &a.PrevHash, &a.Hash, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// --- Device config versioning ---

func (d *DB) CreateDeviceConfig(ctx context.Context, tenantID string, c *store.DeviceConfig) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("cfg-%d", time.Now().UnixNano())
	}
	// Auto-increment version per device+tenant.
	var maxVersion int
	err := d.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM device_configs WHERE device_imei=$1 AND tenant_id=$2",
		c.DeviceIMEI, tenantID,
	).Scan(&maxVersion)
	if err != nil {
		return fmt.Errorf("get max version: %w", err)
	}
	c.Version = maxVersion + 1

	_, err = d.pool.Exec(ctx,
		`INSERT INTO device_configs (id, device_imei, version, config, author, comment, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.ID, c.DeviceIMEI, c.Version, c.Config, c.Author, c.Comment, tenantID)
	return err
}

func (d *DB) GetDeviceConfigLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 ORDER BY version DESC LIMIT 1",
		deviceIMEI, tenantID,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) GetDeviceConfigVersion(ctx context.Context, tenantID string, deviceIMEI string, version int) (*store.DeviceConfig, error) {
	var c store.DeviceConfig
	err := d.pool.QueryRow(ctx,
		"SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 AND version=$3",
		deviceIMEI, tenantID, version,
	).Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *DB) ListDeviceConfigVersions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]store.DeviceConfig, error) {
	query := "SELECT id, device_imei, version, config, author, comment, created_at FROM device_configs WHERE device_imei=$1 AND tenant_id=$2 ORDER BY version DESC"
	args := []interface{}{deviceIMEI, tenantID}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", 3)
		args = append(args, limit)
	}
	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []store.DeviceConfig
	for rows.Next() {
		var c store.DeviceConfig
		if err := rows.Scan(&c.ID, &c.DeviceIMEI, &c.Version, &c.Config, &c.Author, &c.Comment, &c.CreatedAt); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, nil
}

// --- API keys ---

func (d *DB) CreateAPIKey(ctx context.Context, tenantID string, k *store.APIKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("key-%d", time.Now().UnixNano())
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO api_keys (id, key_hash, key_prefix, role, label, device_imei, expires_at, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		k.ID, k.KeyHash, k.KeyPrefix, k.Role, k.Label, k.DeviceIMEI, k.ExpiresAt, tenantID)
	return err
}

func (d *DB) GetAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKey, string, error) {
	var k store.APIKey
	var tenantID string
	err := d.pool.QueryRow(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, created_at, tenant_id FROM api_keys WHERE key_hash=$1",
		keyHash,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt, &tenantID)
	if err != nil {
		return nil, "", err
	}
	return &k, tenantID, nil
}

func (d *DB) ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error) {
	rows, err := d.pool.Query(ctx,
		"SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, created_at FROM api_keys WHERE tenant_id=$1 ORDER BY created_at DESC",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (d *DB) DeleteAPIKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.pool.Exec(ctx, "DELETE FROM api_keys WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

func (d *DB) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, "UPDATE api_keys SET last_used=NOW() WHERE id=$1", id)
	return err
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
	_, err = d.pool.Exec(ctx,
		"INSERT INTO escalation_chains (id, name, tiers, tenant_id) VALUES ($1, $2, $3, $4)",
		c.ID, c.Name, tiersJSON, tenantID)
	return err
}

func (d *DB) GetEscalationChain(ctx context.Context, tenantID string, id string) (*store.EscalationChain, error) {
	var c store.EscalationChain
	var tiersJSON []byte
	query := "SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE id=$1"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=$2"
		args = append(args, tenantID)
	}
	if err := d.pool.QueryRow(ctx, query, args...).Scan(
		&c.ID, &c.Name, &tiersJSON, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(tiersJSON, &c.Tiers)
	return &c, nil
}

func (d *DB) ListEscalationChains(ctx context.Context, tenantID string) ([]store.EscalationChain, error) {
	rows, err := d.pool.Query(ctx,
		"SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE tenant_id=$1 ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	_, err := d.pool.Exec(ctx, "DELETE FROM escalation_chains WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// --- Alerts ---

func (d *DB) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO alerts (id, chain_id, device_imei, type, detail, state, current_tier, retries,
			acked_by, acked_at, next_esc_at, created_at, updated_at, tenant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		a.ID, a.ChainID, a.DeviceIMEI, a.Type, a.Detail, a.State, a.CurrentTier, a.Retries,
		a.AckedBy, a.AckedAt, a.NextEscAt, a.CreatedAt, a.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetAlert(ctx context.Context, tenantID string, id string) (*store.Alert, error) {
	var a store.Alert
	query := "SELECT id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts WHERE id=$1"
	args := []interface{}{id}
	if tenantID != "" {
		query += " AND tenant_id=$2"
		args = append(args, tenantID)
	}
	if err := d.pool.QueryRow(ctx, query, args...).Scan(
		&a.ID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
		&a.CurrentTier, &a.Retries, &a.AckedBy, &a.AckedAt, &a.NextEscAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) ListAlerts(ctx context.Context, tenantID string, activeOnly bool, limit int) ([]store.Alert, error) {
	query := "SELECT id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at FROM alerts"
	var args []interface{}
	argN := 1
	var conditions []string
	if tenantID != "" {
		conditions = append(conditions, fmt.Sprintf("tenant_id=$%d", argN))
		args = append(args, tenantID)
		argN++
	}
	if activeOnly {
		conditions = append(conditions, "state IN ('triggered', 'escalating')")
	}
	if len(conditions) > 0 {
		query += " WHERE " + conditions[0]
		for _, c := range conditions[1:] {
			query += " AND " + c
		}
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argN)
	args = append(args, limit)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var alerts []store.Alert
	for rows.Next() {
		var a store.Alert
		if err := rows.Scan(
			&a.ID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
			&a.CurrentTier, &a.Retries, &a.AckedBy, &a.AckedAt, &a.NextEscAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (d *DB) UpdateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	query := `UPDATE alerts SET state=$1, current_tier=$2, retries=$3, acked_by=$4, acked_at=$5,
		next_esc_at=$6, updated_at=$7 WHERE id=$8`
	args := []interface{}{
		a.State, a.CurrentTier, a.Retries, a.AckedBy, a.AckedAt,
		a.NextEscAt, a.UpdatedAt, a.ID,
	}
	if tenantID != "" {
		query += " AND tenant_id=$9"
		args = append(args, tenantID)
	}
	_, err := d.pool.Exec(ctx, query, args...)
	return err
}

// Compile-time check.
var _ store.Store = (*DB)(nil)
