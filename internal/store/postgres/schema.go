package postgres

// schemaV1 is the Postgres end-state of the MariaDB schema at the time of the
// port (every CREATE TABLE plus the ALTER TABLE columns that followed), as one
// versioned migration. Translation rules: DATETIME -> TIMESTAMPTZ,
// TINYINT(1) -> BOOLEAN, DOUBLE -> DOUBLE PRECISION, JSON -> JSONB,
// MEDIUMTEXT -> TEXT, inline INDEX -> CREATE INDEX, ON UPDATE CURRENT_TIMESTAMP
// dropped (the store sets updated_at explicitly). The credentials table exists
// only in the sqlite store today; it is part of v1 here so MESHSAT-356 works.
//
// Migrations are append-only: never edit an entry once it has been applied
// anywhere; add a new version instead (the runner verifies checksums).
var migrations = []migration{
	{Version: 1, Name: "initial_schema", SQL: `
CREATE TABLE IF NOT EXISTS devices (
	imei VARCHAR(64) PRIMARY KEY,
	label VARCHAR(255) NOT NULL DEFAULT '',
	type VARCHAR(64) NOT NULL DEFAULT 'rockblock',
	notes TEXT NOT NULL DEFAULT '',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	bridge_id VARCHAR(64) NULL,
	last_seen TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_devices_tenant ON devices (tenant_id);

CREATE TABLE IF NOT EXISTS messages (
	id VARCHAR(64) PRIMARY KEY,
	device_imei VARCHAR(64) NOT NULL,
	direction VARCHAR(8) NOT NULL,
	channel VARCHAR(32) NOT NULL DEFAULT 'iridium',
	momsn INTEGER NOT NULL DEFAULT 0,
	text TEXT NOT NULL DEFAULT '',
	raw_hex TEXT NOT NULL DEFAULT '',
	compressed BOOLEAN NOT NULL DEFAULT FALSE,
	status VARCHAR(32) NOT NULL DEFAULT 'received',
	error TEXT NOT NULL DEFAULT '',
	lat DOUBLE PRECISION NOT NULL DEFAULT 0,
	lon DOUBLE PRECISION NOT NULL DEFAULT 0,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	scheduled_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_messages_device ON messages (device_imei, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_tenant ON messages (tenant_id, device_imei);
CREATE INDEX IF NOT EXISTS idx_messages_scheduled ON messages (status, scheduled_at) WHERE status = 'scheduled';

CREATE TABLE IF NOT EXISTS webhook_configs (
	id VARCHAR(64) PRIMARY KEY,
	url TEXT NOT NULL,
	secret VARCHAR(255) NOT NULL DEFAULT '',
	events JSONB NOT NULL DEFAULT '[]'::jsonb,
	max_retries INTEGER NOT NULL DEFAULT 3,
	timeout_sec INTEGER NOT NULL DEFAULT 10,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_configs_tenant ON webhook_configs (tenant_id);

CREATE TABLE IF NOT EXISTS delivery_logs (
	id VARCHAR(64) PRIMARY KEY,
	webhook_id VARCHAR(64) NOT NULL,
	event VARCHAR(64) NOT NULL,
	device_imei VARCHAR(64) NOT NULL DEFAULT '',
	status_code INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL DEFAULT 0,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_delivery_logs_tenant ON delivery_logs (tenant_id);

CREATE TABLE IF NOT EXISTS positions (
	id VARCHAR(64) PRIMARY KEY,
	device_imei VARCHAR(64) NOT NULL,
	lat DOUBLE PRECISION NOT NULL,
	lon DOUBLE PRECISION NOT NULL,
	alt DOUBLE PRECISION NOT NULL DEFAULT 0,
	speed DOUBLE PRECISION NOT NULL DEFAULT 0,
	heading DOUBLE PRECISION NOT NULL DEFAULT 0,
	sats INTEGER NOT NULL DEFAULT 0,
	source VARCHAR(32) NOT NULL DEFAULT 'gps',
	cep DOUBLE PRECISION NOT NULL DEFAULT 0,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_positions_device ON positions (device_imei, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_positions_tenant ON positions (tenant_id, device_imei);

CREATE TABLE IF NOT EXISTS audit_log (
	id VARCHAR(64) PRIMARY KEY,
	action VARCHAR(64) NOT NULL,
	actor VARCHAR(255) NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	ip VARCHAR(64) NOT NULL DEFAULT '',
	prev_hash VARCHAR(64) NOT NULL DEFAULT '',
	hash VARCHAR(64) NOT NULL DEFAULT '',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_log_tenant ON audit_log (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
	id VARCHAR(64) PRIMARY KEY,
	key_hash VARCHAR(64) NOT NULL UNIQUE,
	key_prefix VARCHAR(32) NOT NULL DEFAULT '',
	role VARCHAR(16) NOT NULL DEFAULT 'viewer',
	label VARCHAR(255) NOT NULL DEFAULT '',
	device_imei VARCHAR(64) NOT NULL DEFAULT '',
	last_used TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
	expires_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
	rotation_days INTEGER NOT NULL DEFAULT 0,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys (tenant_id);

CREATE TABLE IF NOT EXISTS device_configs (
	id VARCHAR(64) PRIMARY KEY,
	device_imei VARCHAR(64) NOT NULL,
	version INTEGER NOT NULL DEFAULT 1,
	config TEXT NOT NULL DEFAULT '',
	author VARCHAR(255) NOT NULL DEFAULT '',
	comment TEXT NOT NULL DEFAULT '',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (device_imei, version, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_device_configs_device ON device_configs (device_imei, tenant_id, version DESC);

CREATE TABLE IF NOT EXISTS escalation_chains (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL DEFAULT '',
	tiers JSONB NOT NULL DEFAULT '[]'::jsonb,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_escalation_chains_tenant ON escalation_chains (tenant_id);

CREATE TABLE IF NOT EXISTS alerts (
	id VARCHAR(64) PRIMARY KEY,
	chain_id VARCHAR(64) NOT NULL DEFAULT '',
	device_imei VARCHAR(64) NOT NULL DEFAULT '',
	type VARCHAR(32) NOT NULL DEFAULT 'sos',
	detail TEXT NOT NULL DEFAULT '',
	state VARCHAR(32) NOT NULL DEFAULT 'triggered',
	current_tier INTEGER NOT NULL DEFAULT 0,
	retries INTEGER NOT NULL DEFAULT 0,
	acked_by VARCHAR(255) NOT NULL DEFAULT '',
	acked_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
	next_esc_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_alerts_tenant ON alerts (tenant_id);
CREATE INDEX IF NOT EXISTS idx_alerts_state ON alerts (state, next_esc_at);

CREATE TABLE IF NOT EXISTS notification_prefs (
	device_imei VARCHAR(64) NOT NULL DEFAULT '*',
	urls JSONB NOT NULL DEFAULT '[]'::jsonb,
	events JSONB NOT NULL DEFAULT '[]'::jsonb,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (device_imei, tenant_id)
);

CREATE TABLE IF NOT EXISTS users (
	id VARCHAR(64) PRIMARY KEY,
	email VARCHAR(255) NOT NULL,
	name VARCHAR(255) NOT NULL DEFAULT '',
	password_hash VARCHAR(255) NOT NULL,
	role VARCHAR(16) NOT NULL DEFAULT 'viewer',
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	failed_logins INTEGER NOT NULL DEFAULT 0,
	locked_until TIMESTAMPTZ NULL,
	last_login_at TIMESTAMPTZ NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (email, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_users_tenant ON users (tenant_id);
CREATE INDEX IF NOT EXISTS idx_users_email_lower ON users (LOWER(email), tenant_id);

CREATE TABLE IF NOT EXISTS refresh_tokens (
	id VARCHAR(64) PRIMARY KEY,
	user_id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	token_hash VARCHAR(64) NOT NULL UNIQUE,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens (user_id, tenant_id);

CREATE TABLE IF NOT EXISTS device_keys (
	id VARCHAR(64) PRIMARY KEY,
	device_imei VARCHAR(20) NOT NULL,
	key_hash VARCHAR(64) NOT NULL,
	key_hex VARCHAR(64) NOT NULL DEFAULT '',
	mode VARCHAR(16) NOT NULL DEFAULT 'decrypt',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default'
);
CREATE INDEX IF NOT EXISTS idx_device_keys_device ON device_keys (device_imei, tenant_id);

CREATE TABLE IF NOT EXISTS device_wireguard (
	device_imei VARCHAR(64) NOT NULL,
	peer_id VARCHAR(64) NOT NULL DEFAULT '',
	vpn_address VARCHAR(64) NOT NULL DEFAULT '',
	public_key VARCHAR(255) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	PRIMARY KEY (device_imei, tenant_id)
);

CREATE TABLE IF NOT EXISTS routes (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL DEFAULT '',
	source_type VARCHAR(64) NOT NULL DEFAULT '',
	destination_type VARCHAR(64) NOT NULL DEFAULT '',
	filter TEXT NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default'
);
CREATE INDEX IF NOT EXISTS idx_routes_tenant ON routes (tenant_id);

CREATE TABLE IF NOT EXISTS system_config (
	key VARCHAR(255) PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bridges (
	bridge_id VARCHAR(64) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	label VARCHAR(255) NOT NULL DEFAULT '',
	hostname VARCHAR(255) NOT NULL DEFAULT '',
	version VARCHAR(64) NOT NULL DEFAULT '',
	mode VARCHAR(32) NOT NULL DEFAULT 'direct',
	location_lat DOUBLE PRECISION NOT NULL DEFAULT 0,
	location_lon DOUBLE PRECISION NOT NULL DEFAULT 0,
	location_alt DOUBLE PRECISION NOT NULL DEFAULT 0,
	capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
	reticulum_hash VARCHAR(64) NOT NULL DEFAULT '',
	reticulum_pubkey TEXT NOT NULL DEFAULT '',
	cot_type VARCHAR(32) NOT NULL DEFAULT 'a-f-G-U-C-I',
	cot_callsign VARCHAR(64) NOT NULL DEFAULT '',
	online BOOLEAN NOT NULL DEFAULT FALSE,
	last_birth JSONB NOT NULL DEFAULT '{}'::jsonb,
	last_health JSONB NOT NULL DEFAULT '{}'::jsonb,
	last_seen TIMESTAMPTZ NULL,
	mqtt_username VARCHAR(64) NOT NULL DEFAULT '',
	mqtt_password_hash VARCHAR(255) NOT NULL DEFAULT '',
	cert_pem TEXT NOT NULL DEFAULT '',
	cert_expiry TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_bridges_tenant ON bridges (tenant_id);
CREATE INDEX IF NOT EXISTS idx_bridges_online ON bridges (online);

CREATE TABLE IF NOT EXISTS cost_ledger (
	id VARCHAR(64) PRIMARY KEY,
	device_imei VARCHAR(64) NOT NULL DEFAULT '',
	interface_type VARCHAR(32) NOT NULL DEFAULT '',
	direction VARCHAR(8) NOT NULL DEFAULT 'mt',
	cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	message_id VARCHAR(64) NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cost_ledger_tenant ON cost_ledger (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cost_ledger_device ON cost_ledger (device_imei, tenant_id);

CREATE TABLE IF NOT EXISTS device_groups (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	color VARCHAR(16) NOT NULL DEFAULT '#6b7280',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_device_groups_tenant ON device_groups (tenant_id);

CREATE TABLE IF NOT EXISTS device_group_members (
	group_id VARCHAR(64) NOT NULL,
	device_imei VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	PRIMARY KEY (group_id, device_imei, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_dgm_device ON device_group_members (device_imei, tenant_id);

CREATE TABLE IF NOT EXISTS message_templates (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	variables TEXT NOT NULL DEFAULT '',
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_message_templates_tenant ON message_templates (tenant_id);

CREATE TABLE IF NOT EXISTS alert_rules (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL DEFAULT '',
	condition_type VARCHAR(64) NOT NULL DEFAULT '',
	condition_params TEXT NOT NULL DEFAULT '',
	chain_id VARCHAR(64) NOT NULL DEFAULT '',
	device_filter VARCHAR(255) NOT NULL DEFAULT '*',
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	last_evaluated TIMESTAMPTZ NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_alert_rules_tenant ON alert_rules (tenant_id);

CREATE TABLE IF NOT EXISTS bond_groups (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	bridge_id VARCHAR(128) NOT NULL,
	label VARCHAR(255) NOT NULL DEFAULT '',
	members TEXT NOT NULL DEFAULT '',
	cost_budget DOUBLE PRECISION NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, bridge_id, id)
);

CREATE TABLE IF NOT EXISTS credentials (
	id VARCHAR(64) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	provider VARCHAR(64) NOT NULL,
	name VARCHAR(255) NOT NULL,
	cred_type VARCHAR(32) NOT NULL,
	encrypted_data BYTEA NOT NULL,
	cert_not_after TIMESTAMPTZ NULL,
	cert_subject TEXT NOT NULL DEFAULT '',
	cert_issuer TEXT NOT NULL DEFAULT '',
	cert_fingerprint VARCHAR(128) NOT NULL DEFAULT '',
	target_scope VARCHAR(16) NOT NULL DEFAULT 'hub',
	target_bridge_id VARCHAR(64) NOT NULL DEFAULT '',
	status VARCHAR(16) NOT NULL DEFAULT 'active',
	version INTEGER NOT NULL DEFAULT 1,
	distributed_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credentials_tenant ON credentials (tenant_id);
`},
	{Version: 2, Name: "claims_and_deadman", SQL: `
CREATE TABLE IF NOT EXISTS dispatch_claims (
	key VARCHAR(255) PRIMARY KEY,
	claimed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_dispatch_claims_at ON dispatch_claims (claimed_at);
ALTER TABLE messages ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ NULL;
CREATE TABLE IF NOT EXISTS deadman_configs (
	device_imei VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	chain_id VARCHAR(64) NOT NULL DEFAULT '',
	interval_sec INTEGER NOT NULL DEFAULT 3600,
	grace_sec INTEGER NOT NULL DEFAULT 600,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	snoozed_until TIMESTAMPTZ NULL,
	alerted BOOLEAN NOT NULL DEFAULT FALSE,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (device_imei, tenant_id)
);
`},
	{Version: 3, Name: "tenants_and_invites", SQL: `
CREATE TABLE IF NOT EXISTS tenants (
	id VARCHAR(64) PRIMARY KEY,
	slug VARCHAR(64) NOT NULL UNIQUE,
	name VARCHAR(255) NOT NULL DEFAULT '',
	owner_user_id VARCHAR(64) NOT NULL DEFAULT '',
	plan VARCHAR(32) NOT NULL DEFAULT 'beta',
	status VARCHAR(32) NOT NULL DEFAULT 'active',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO tenants (id, slug, name) VALUES ('default', 'default', 'Default') ON CONFLICT (id) DO NOTHING;
CREATE TABLE IF NOT EXISTS tenant_invites (
	id VARCHAR(64) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL,
	email_lower VARCHAR(255) NOT NULL,
	role VARCHAR(16) NOT NULL DEFAULT 'viewer',
	token_hash VARCHAR(64) NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ NOT NULL,
	accepted_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tenant_invites_email ON tenant_invites (email_lower, accepted_at);
CREATE INDEX IF NOT EXISTS idx_tenant_invites_tenant ON tenant_invites (tenant_id);
CREATE TABLE IF NOT EXISTS oidc_identities (
	issuer VARCHAR(255) NOT NULL,
	subject VARCHAR(255) NOT NULL,
	user_id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	email VARCHAR(255) NOT NULL DEFAULT '',
	platform_admin BOOLEAN NOT NULL DEFAULT FALSE,
	last_login_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (issuer, subject)
);
`},
	{Version: 4, Name: "route_senders", SQL: `
ALTER TABLE routes ADD COLUMN IF NOT EXISTS senders TEXT NOT NULL DEFAULT '';
`},
	{Version: 5, Name: "bridge_last_report", SQL: `
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS last_report_bearer TEXT NOT NULL DEFAULT '';
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS last_report_at TIMESTAMPTZ NULL;
`},
	{Version: 6, Name: "bridge_oob_peers", SQL: `
CREATE TABLE IF NOT EXISTS bridge_oob_peers (
	tenant_id VARCHAR(64) NOT NULL,
	bridge_id VARCHAR(64) NOT NULL,
	peer_id INTEGER NOT NULL,
	key_enc BYTEA NOT NULL,
	local_role SMALLINT NOT NULL DEFAULT 0,
	phone VARCHAR(32) NOT NULL DEFAULT '',
	sat_imei VARCHAR(32) NOT NULL DEFAULT '',
	tx_counter BIGINT NOT NULL DEFAULT 0,
	rx_high BIGINT NOT NULL DEFAULT 0,
	rx_window BIGINT NOT NULL DEFAULT 0,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, bridge_id)
);
CREATE INDEX IF NOT EXISTS idx_bridge_oob_peers_peer ON bridge_oob_peers (peer_id);
`},
	{Version: 7, Name: "tenant_soft_delete", SQL: `
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
		CREATE INDEX IF NOT EXISTS idx_tenants_deleted_at ON tenants (deleted_at) WHERE deleted_at IS NOT NULL;
	`},
}
