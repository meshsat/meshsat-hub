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
// THE TEXT OF AN APPLIED MIGRATION IS IMMUTABLE, COMMENTS INCLUDED. The runner
// hashes each applied migration's SQL and refuses to boot on a mismatch
// ("applied migration text changed"). On 2026-09-14 an explanatory SQL comment
// added INSIDE migration 1's string crashlooped the new Hub pod on production
// (old pods kept serving; the rollout stuck). Critical rule 9 means the string,
// not just the schema. Explain things here, in Go comments, never in the SQL.
//
// devices.imei is the PRIMARY KEY on its own, NOT (tenant_id, imei), and that is
// load-bearing rather than an inconsistency with geofences and webhook_configs.
// A satellite provider delivers an MO by IMEI with no tenant context, so
// LookupDeviceTenant must answer "whose is this?" from the IMEI alone; if two
// tenants could hold one IMEI the resolver sees ErrAmbiguousTenant and falls
// back to the PLATFORM tenant, filing a customer's positions, messages and SOS
// under the operator's own account. Re-keying was drafted and reverted on
// 2026-09-14 for exactly that reason. One physical modem, one owner. A resold
// kit is a transfer, not a second registration; the API tells the second
// tenant so instead of leaking a database error.
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
	// MESHSAT-989: subscription tiers.
	//
	// plan_expires_at rather than a subscription state, because Ko-fi fires a
	// webhook on payment and never on cancellation. A payment pushes the date
	// out; a plan lapses by the date passing. There is nothing to miss.
	//
	// kofi_claim_code matches a payment to a tenant: a supporter puts it in
	// the Ko-fi message, because people pay from a different address than they
	// signed up with often enough that email alone loses payments.
	{Version: 8, Name: "tenant_subscription", SQL: `
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS plan_expires_at TIMESTAMPTZ;
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS kofi_claim_code VARCHAR(16) NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS idx_tenants_plan_expires ON tenants (plan_expires_at) WHERE plan_expires_at IS NOT NULL;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_kofi_claim ON tenants (kofi_claim_code) WHERE kofi_claim_code <> '';
	`},
	// MESHSAT-989: remember who paid.
	//
	// Ko-fi sends the supporter's message only when they join. Every renewal
	// after that arrives with message null, so the claim code that matched the
	// first payment is not there on the second. Without somewhere to remember
	// the payer, a subscriber whose Ko-fi address differs from their account
	// address -- the exact case claim codes exist for -- would match once, then
	// silently lapse a month later while still paying.
	{Version: 9, Name: "tenant_kofi_payer", SQL: `
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS kofi_payer_email VARCHAR(254) NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS idx_tenants_kofi_payer ON tenants (kofi_payer_email) WHERE kofi_payer_email <> '';
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS kofi_last_message_id VARCHAR(64) NOT NULL DEFAULT '';
	`},
	{Version: 10, Name: "receipts", SQL: `
CREATE TABLE IF NOT EXISTS receipts (
	id VARCHAR(64) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL,
	delivery_key VARCHAR(255) NOT NULL UNIQUE,
	transaction_id VARCHAR(255) NOT NULL DEFAULT '',
	email VARCHAR(254) NOT NULL DEFAULT '',
	name VARCHAR(255) NOT NULL DEFAULT '',
	amount_cents BIGINT NOT NULL DEFAULT 0,
	currency VARCHAR(8) NOT NULL DEFAULT '',
	plan VARCHAR(32) NOT NULL DEFAULT '',
	tier_name VARCHAR(255) NOT NULL DEFAULT '',
	paid_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	status VARCHAR(16) NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	invoice_number VARCHAR(64) NOT NULL DEFAULT '',
	invoice_ref VARCHAR(64) NOT NULL DEFAULT '',
	issued_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_receipts_due ON receipts (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_receipts_tenant ON receipts (tenant_id);
	`},
	// v11 gives the plan grant the durable idempotency the receipts outbox
	// already had. Ko-fi retries until it gets a 200 and does not always send a
	// message_id; the old guard compared that empty id to an empty column,
	// matched nothing, and bought another 32 days on every retry.
	{Version: 11, Name: "kofi_deliveries", SQL: `
CREATE TABLE IF NOT EXISTS kofi_deliveries (
    delivery_key VARCHAR(128) PRIMARY KEY,
    tenant_id    VARCHAR(64)  NOT NULL,
    applied_at   TIMESTAMPTZ  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kofi_deliveries_tenant ON kofi_deliveries (tenant_id, applied_at);
`},
	// v12 remembers that a lapse warning was sent, so the hourly job does not
	// mail hourly. It counts for the expiry it was sent for, so a renewal that
	// pushes the date out re-arms the warning.
	{Version: 12, Name: "tenant_lapse_warned_at", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS lapse_warned_at TIMESTAMPTZ;
`},
	// A receipt draws an invoice number out of a gapless legal series, so two
	// drainers working one row would create two documents and leave the first
	// permanently unpaid in the books. Until now only timing stopped that: one
	// leader, a stop wait longer than the billing call, a pod grace period
	// longer than the stop wait. leased_until makes it the database's job
	// instead, like every other money guard here (MESHSAT-998).
	{Version: 13, Name: "receipt_row_lease", SQL: `
ALTER TABLE receipts ADD COLUMN IF NOT EXISTS leased_until TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00';
`},
	// Where the buyer is decides whether Dutch VAT applies at all, and until
	// now nothing anywhere knew: every customer was written into the billing
	// system as Dutch and charged 21%. Right for the Netherlands, defensible
	// inside the EU under the Article 59c threshold, and wrong for anybody
	// outside it. billing_country is ISO 3166-1 alpha-2, empty when unknown;
	// billing_country_evidence records what it was derived from, because the
	// rules ask for evidence of a consumer's location rather than a guess
	// (MESHSAT-1016).
	{Version: 14, Name: "buyer_country", SQL: `
ALTER TABLE tenants  ADD COLUMN IF NOT EXISTS billing_country VARCHAR(2) NOT NULL DEFAULT '';
ALTER TABLE tenants  ADD COLUMN IF NOT EXISTS billing_country_evidence VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE receipts ADD COLUMN IF NOT EXISTS country VARCHAR(2) NOT NULL DEFAULT '';
`},
	// The terms promise EU consumers 14 days to withdraw and get their money
	// back, and a Ko-fi payment is refundable in the payment processor. Nothing
	// produced a document when that happened: the processor's refund emails are
	// off by deliberate setting and the billing system had no credit-note flow,
	// so the sale stayed in the books at full value with its VAT declared.
	//
	// receipt_id is UNIQUE. A payment is refunded once; a second row against
	// the same payment would credit money nobody paid twice (MESHSAT-1019).
	{Version: 15, Name: "refunds", SQL: `
CREATE TABLE IF NOT EXISTS refunds (
	id VARCHAR(64) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL,
	receipt_id VARCHAR(64) NOT NULL UNIQUE,
	amount_cents BIGINT NOT NULL DEFAULT 0,
	currency VARCHAR(8) NOT NULL DEFAULT '',
	country VARCHAR(2) NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	requested_by VARCHAR(254) NOT NULL DEFAULT '',
	refunded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	status VARCHAR(16) NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	credit_number VARCHAR(64) NOT NULL DEFAULT '',
	credit_ref VARCHAR(64) NOT NULL DEFAULT '',
	issued_at TIMESTAMPTZ NULL,
	leased_until TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_refunds_due ON refunds (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_refunds_tenant ON refunds (tenant_id);
`},
	// v16: Stripe replaces Ko-fi as the payment provider (MESHSAT-1023).
	//
	// stripe_events carries tenant_id even though the primary key is the event
	// id, and that column is load-bearing: the tenant export and purge discover
	// the tables they must cover by looking for a tenant_id column, and never
	// name one in code. A table without it silently stops being erased.
	//
	// The kofi_* columns are deliberately left in place. Migrations are
	// append-only here, nobody has paid through Ko-fi, and dropping them buys
	// nothing but a way to lose data.
	{Version: 16, Name: "stripe", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS stripe_customer_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS stripe_subscription_id VARCHAR(64) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_stripe_customer ON tenants (stripe_customer_id) WHERE stripe_customer_id <> '';
CREATE TABLE IF NOT EXISTS stripe_events (
	event_id VARCHAR(128) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_stripe_events_tenant ON stripe_events (tenant_id, applied_at);
`},
	// v17: remember which provider payment settled a receipt (MESHSAT-1023).
	//
	// Stripe's charge no longer carries the invoice it paid, and neither the
	// charge nor the payment intent links back to one, so a refunded
	// subscription matched no receipt and issued no credit note -- the sale
	// stayed in the books at full value with its VAT declared. The link is
	// recorded when the payment happens rather than reconstructed under
	// pressure at refund time.
	//
	// Nullable-by-default and empty for every existing row, which is correct:
	// those were settled before anything recorded this, and a refund of one
	// still falls through to the older matching paths.
	{Version: 17, Name: "receipt payment ref", SQL: `
ALTER TABLE receipts ADD COLUMN IF NOT EXISTS payment_ref VARCHAR(128) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_receipts_payment_ref ON receipts (payment_ref) WHERE payment_ref <> '';
`},
	// v18: hosted per-tenant TAK (MESHSAT-1037).
	//
	// Two tables, and BOTH carry tenant_id because that column is not
	// decoration: the tenant export and the purge job discover which tables
	// belong to a tenant by reflecting on it
	// (postgres/tenant_lifecycle.go: information_schema.columns WHERE
	// column_name = 'tenant_id'). A table without it is silently never
	// exported and never erased, which is the quiet way to keep a customer's
	// data after they asked for it to be destroyed.
	//
	// tak_instances is the Hub's record of a tenant's OpenTAKServer. The
	// operator owns the real state in the TakInstance custom resource; this
	// row is what the Hub reads to build the takfront directory without a
	// Kubernetes call on the request path, plus the CA certificate it must
	// trust. label is the opaque ten-character instance name -- never the
	// tenant's slug, because a CA subject travels to every phone and must leak
	// nothing about who the customer is.
	//
	// tak_users is one TAK account. enroll_token_hash holds a SHA-256 of a
	// single-use enrollment token, never the token: it is emailed or shown as
	// a QR once, and a stored token would be a credential sitting in a backup.
	// It is also in store.RedactedInExport for the same reason.
	//
	// revoked_serial is the certificate serial the Hub refuses after a user is
	// removed. There is no CRL: the front asks the Authorizer on every
	// connection, so refusing a serial here is immediate and needs no
	// distribution.
	{Version: 18, Name: "hosted tak", SQL: `
CREATE TABLE IF NOT EXISTS tak_instances (
	tenant_id VARCHAR(64) PRIMARY KEY,
	label VARCHAR(32) NOT NULL,
	state VARCHAR(16) NOT NULL DEFAULT 'Running',
	phase VARCHAR(32) NOT NULL DEFAULT '',
	host VARCHAR(253) NOT NULL DEFAULT '',
	ca_cert_pem TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tak_instances_label ON tak_instances (label);
CREATE TABLE IF NOT EXISTS tak_users (
	tenant_id VARCHAR(64) NOT NULL,
	username VARCHAR(32) NOT NULL,
	callsign VARCHAR(64) NOT NULL DEFAULT '',
	active BOOLEAN NOT NULL DEFAULT true,
	cert_serial VARCHAR(64) NOT NULL DEFAULT '',
	cert_not_after TIMESTAMPTZ,
	revoked_serial VARCHAR(64) NOT NULL DEFAULT '',
	enroll_token_hash VARCHAR(64) NOT NULL DEFAULT '',
	enroll_expires_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, username)
);
CREATE INDEX IF NOT EXISTS idx_tak_users_serial ON tak_users (cert_serial) WHERE cert_serial <> '';
CREATE INDEX IF NOT EXISTS idx_tak_users_enroll ON tak_users (enroll_token_hash) WHERE enroll_token_hash <> '';
`},
	// v19: the first tenant-owned operational setting (MESHSAT-1117).
	//
	// HUB_BRIDGE_OFFLINE_TIMEOUT decides how long a bridge may go quiet before
	// the Hub calls it offline. That is a property of how a particular fleet
	// operates -- a vehicle on a city network and a kit on a satellite schedule
	// do not want the same number -- and it was one global environment variable
	// for every tenant on a multi-tenant platform, changeable only by editing a
	// ConfigMap.
	//
	// 0 means "use the platform default", which is what every existing tenant
	// gets and is why this can be added without touching a single row. The
	// platform still bounds what an owner may choose
	// (HUB_BRIDGE_OFFLINE_TIMEOUT_MIN / _MAX): too low and the reaper flaps a
	// healthy fleet offline between heartbeats, too high and a dead bridge
	// reads as online for a day.
	{Version: 19, Name: "tenant bridge offline timeout", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS bridge_offline_timeout INTEGER NOT NULL DEFAULT 0;
`},
	// v20: audit retention, per tenant (MESHSAT-1117 tranche 2b).
	//
	// HUB_AUDIT_RETENTION_DAYS decided how long EVERY tenant's audit log lived.
	// An audit log is the customer's own record of who did what in their
	// account, and how long they need to keep it is their compliance question,
	// not ours.
	//
	// 0 means "use the platform default", as with bridge_offline_timeout. The
	// platform still bounds the choice, and the floor is the load-bearing half:
	// without one a tenant could set a retention so short that the evidence of
	// a security event in their own account is gone before anyone looks. The
	// ceiling is the boring half -- storage here is node-local with no
	// replicated block storage, and audit rows are an append-only hash chain
	// that only ever grows.
	{Version: 20, Name: "tenant audit retention", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS audit_retention_days INTEGER NOT NULL DEFAULT 0;
`},
	// v21: per-tenant send budget (MESHSAT-1117 tranche 2c).
	//
	// Unlike the two settings before it, these are NOT for a tenant owner to
	// set: a send budget is a commercial lever and self-service would make it
	// free. They are a PLATFORM ADMIN's override on top of the plan-derived
	// value, for the customer who has a reason and an invoice.
	//
	// 0 means "no override, use the plan", and the plan in turn falls back to
	// the platform default. A resolved budget is never lower than the platform
	// default -- see plans.SendCaps for why that floor is not optional.
	{Version: 21, Name: "tenant send caps", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS ratelimit_daily_cap INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS ratelimit_monthly_cap INTEGER NOT NULL DEFAULT 0;
`},
	// v22: geofences get a table (MESHSAT-1119).
	//
	// They never had one. The engine was built in main.go, handed to the API
	// handler and never shown a position: a fence could be created, listed and
	// deleted, it was never compared against anything, and it vanished at the
	// next rollout. A top-level nav entry and a form asking which escalation
	// chain to trigger, in front of nothing.
	//
	// PRIMARY KEY (tenant_id, id), not id alone. That is the lesson from
	// webhook_configs, whose id IS the key: two tenants naming a fence
	// "perimeter" must be two rows, and an upsert must never reach across a
	// tenant boundary.
	{Version: 22, Name: "geofences", SQL: `
CREATE TABLE IF NOT EXISTS geofences (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
	name VARCHAR(255) NOT NULL DEFAULT '',
	polygon JSONB NOT NULL DEFAULT '[]'::jsonb,
	trigger_mode VARCHAR(16) NOT NULL DEFAULT 'both',
	chain_id VARCHAR(64) NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_geofences_tenant ON geofences (tenant_id);
`},
	// v23: a crossing cooldown, so a fence cannot page somebody all night.
	//
	// MESHSAT-1119 made fences fire. A transition triggers the moment
	// point-in-polygon flips, so a device parked on a boundary -- a vehicle at a
	// depot gate, and GPS jitter is metres -- produces enter/exit/enter/exit, and
	// each one raises the chain and sends a real SMS.
	//
	// This damps the OUTPUT, not the input: the first transition fires
	// IMMEDIATELY and further events for the same device and fence are suppressed
	// for cooldown_sec. Dwell time (the obvious alternative, and what the issue
	// first recommended) damps the input instead, which means waiting for a
	// SECOND report on the new side -- and with Iridium SBD reports minutes
	// apart, that delays every genuine crossing alert by a full reporting
	// interval. On a safety path a late alert is worse than a duplicate one.
	//
	// 0 means the platform default (HUB_GEOFENCE_COOLDOWN_SEC), as with every
	// other setting added by MESHSAT-1117. Per fence rather than per tenant
	// because a depot gate and a national border want different numbers.
	{Version: 23, Name: "geofence cooldown", SQL: `
ALTER TABLE geofences ADD COLUMN IF NOT EXISTS cooldown_sec INTEGER NOT NULL DEFAULT 0;
`},
	// v24: out-of-band command policy, per tenant (MESHSAT-1121).
	//
	// An OOB frame commands a tenant's OWN kit over a bearer the tenant is billed
	// for, so how many may go out in an hour and how long to wait for a reply are
	// the owner's operational choices, not the platform's. They were
	// HUB_OOB_MAX_PER_HOUR, HUB_OOB_SMS_TIMEOUT and HUB_OOB_SAT_TIMEOUT: one set
	// of numbers for everybody, tuned for the operator's two kits and applied to
	// a customer with a hundred.
	//
	// 0 means the platform default, so this migration touches no existing row and
	// a self-hosted single-tenant Hub is unaffected -- the shape every setting
	// moved by MESHSAT-1117 uses.
	//
	// Timeouts are stored in SECONDS rather than as a duration string: the column
	// is then comparable and clampable in SQL, and there is no parse that can
	// fail at read time on a value somebody wrote by hand.
	//
	// There is deliberately no oob_encrypt column. Sealing is not a preference:
	// the frame can reboot or factory-reset hardware in the field over bearers
	// that are not confidential. HUB_OOB_ENCRYPT was deleted rather than moved.
	{Version: 24, Name: "oob policy per tenant", SQL: `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS oob_max_per_hour INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS oob_sms_timeout_sec INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS oob_sat_timeout_sec INTEGER NOT NULL DEFAULT 0;
`},
	// v25: PGP email contacts get a table (MESHSAT-1123).
	//
	// They lived in a Go map and nowhere else, which failed twice over. Every
	// rollout emptied it, and the Hub runs TWO replicas, so a key added through
	// the API was known to whichever pod served the request and not the other.
	//
	// Neither failure raised anything: a recipient with no key on file falls back
	// to CLEARTEXT rather than erroring, so the symptom was alert mail quietly
	// ceasing to be encrypted, depending on the pod and on how recently anything
	// had restarted.
	//
	// PRIMARY KEY (tenant_id, email), not email: the address is the natural key
	// but it is only unique WITHIN a tenant, and a key on email alone is exactly
	// the defect MESHSAT-1121 removed from the in-memory map -- one tenant's
	// contact overwriting another's for the same correspondent.
	//
	// tenant_id is also what the tenant export and purge find their tables by, so
	// a table without it silently stops being erased when an account closes.
	{Version: 25, Name: "email contacts", SQL: `
CREATE TABLE IF NOT EXISTS email_contacts (
	tenant_id VARCHAR(64) NOT NULL,
	email VARCHAR(320) NOT NULL,
	armored_key TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, email)
);
CREATE INDEX IF NOT EXISTS idx_email_contacts_tenant ON email_contacts (tenant_id);
`},
	// The TTC booth flow (MESHSAT-1175). Two tables, both tenant-keyed.
	//
	// booth_relays is the RETURN LEG's correlation, and it is in the database
	// rather than in a map on purpose: both Hub replicas process every message,
	// so the reply coming back off the mesh routinely lands on the pod that did
	// not send the original. A Go map would route correctly about half the time
	// and look haunted.
	//
	// ref is the short token carried in the mesh text ("#A7 ...", bare and
	// at the start): the Bridge rewrites brackets on egress to a plaintext
	// peer, so a bracketed token would not survive the return leg. closed_at
	// being NULL is what "conversation still open" means, and the partial index
	// is what makes the single-open-conversation fallback a cheap lookup rather
	// than a scan: a reply with no ref routes when its (bridge, node) has
	// exactly one open row.
	{Version: 26, Name: "booth relay sessions", SQL: `
CREATE TABLE IF NOT EXISTS booth_sessions (
	tenant_id   VARCHAR(64) NOT NULL,
	sender      VARCHAR(64) NOT NULL,
	channel     VARCHAR(16) NOT NULL,
	state       VARCHAR(32) NOT NULL DEFAULT 'menu',
	opted_in_at TIMESTAMPTZ,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, sender, channel)
);
CREATE TABLE IF NOT EXISTS booth_relays (
	tenant_id  VARCHAR(64) NOT NULL,
	ref        VARCHAR(16) NOT NULL,
	sender     VARCHAR(64) NOT NULL,
	channel    VARCHAR(16) NOT NULL,
	bridge_id  VARCHAR(64) NOT NULL,
	mesh_dest  VARCHAR(64) NOT NULL DEFAULT '',
	body       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL,
	closed_at  TIMESTAMPTZ,
	PRIMARY KEY (tenant_id, ref)
);
CREATE INDEX IF NOT EXISTS idx_booth_relays_open
	ON booth_relays (tenant_id, bridge_id, mesh_dest) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_booth_relays_sender
	ON booth_relays (tenant_id, sender, created_at);
CREATE INDEX IF NOT EXISTS idx_booth_relays_created
	ON booth_relays (tenant_id, created_at);
`},
	// Every message the booth sends, so spend has a ceiling (MESHSAT-1175).
	//
	// The relay quotas count RELAYS, which is not what costs money: a visitor who
	// texts the keyword and browses the menu without ever relaying still sends
	// four messages and hits no limit at all. At NL rates a full visitor run is
	// about EUR 0.80 and the account holds under 100 USD, so an afternoon of
	// curiosity could empty it.
	//
	// A ledger rather than a counter column, matching booth_relays: counting rows
	// cannot drift from what was actually sent, and a window is just a WHERE.
	{Version: 27, Name: "booth send ledger", SQL: `
CREATE TABLE IF NOT EXISTS booth_sends (
	tenant_id  VARCHAR(64) NOT NULL,
	id         VARCHAR(32) NOT NULL,
	recipient  VARCHAR(64) NOT NULL,
	channel    VARCHAR(16) NOT NULL,
	kind       VARCHAR(16) NOT NULL DEFAULT 'visitor',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_booth_sends_recipient
	ON booth_sends (tenant_id, recipient, created_at);
CREATE INDEX IF NOT EXISTS idx_booth_sends_created
	ON booth_sends (tenant_id, created_at);
`},
	// MESHSAT-1181. The Hub had no way to answer "is a node reachable behind
	// this kit?". Bridge liveness is cellular and says nothing about radio, so
	// the booth menu offered meshes that could not answer.
	//
	// Keyed (tenant_id, bridge_id, node_id) rather than on the node alone: one
	// node in range of two kits is two true facts, not a row that flips between
	// them. first_heard is kept because it costs nothing and answers "is this
	// the node that has been here all day, or a visitor's?".
	{Version: 28, Name: "mesh node presence", SQL: `
CREATE TABLE IF NOT EXISTS mesh_nodes (
	tenant_id   VARCHAR(64) NOT NULL,
	bridge_id   VARCHAR(64) NOT NULL,
	node_id     VARCHAR(64) NOT NULL,
	first_heard TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_heard  TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, bridge_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_mesh_nodes_recent
	ON mesh_nodes (tenant_id, bridge_id, last_heard);
`},

	// MESHSAT-1209: an API key may now carry the platform axis.
	//
	// Until this, PlatformAdmin: true could ONLY come from the static
	// HUB_AUTH_TOKEN -- never-expiring, non-revocable, rotated only by redeploy --
	// which is why V6 of the posture scorecard could not move. api_keys already
	// has expires_at and rotation_days, so a key with this flag is the same
	// authority with a revocation path and a clock.
	//
	// DEFAULT false, and the column alone grants nothing: the handler refuses to
	// SET it unless the caller already holds PlatformAdmin, because
	// POST /api/auth/keys is open to any tenant owner. The column is the easy
	// half; the gate is the change.
	{Version: 29, Name: "api key platform admin", SQL: `
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS platform_admin BOOLEAN NOT NULL DEFAULT false;
`},
	// 30 -- the audit chain's two structural gaps (MESHSAT-1215, posture item 6).
	//
	// hash_version: the v1 digest bound action|actor|detail|ip|prev_hash only, so
	// a row could be moved to another tenant or back-dated and still verify. v2
	// also binds tenant_id, id and created_at. The formula change would
	// invalidate every existing row, and the obvious remedy -- a verifier that
	// falls back to v1 when v2 fails -- is itself the bypass (an edited row is
	// simply recomputed the old way). So the version is STORED per row: existing
	// rows keep 1 and verify with v1, new rows are 2, and a 1 after a 2 is a
	// break. DEFAULT 1 is what makes the 231 rows already in production honest
	// about what they were hashed with.
	//
	// The unique index refuses a fork. Appends are serialised by an advisory
	// lock per tenant (AppendAuditEntry), which is what stops two replicas
	// reading the same latest row; the index is the backstop that turns any
	// write that gets past that lock -- a future code path, a hand INSERT --
	// into a loud error instead of two children of one parent that a chain walk
	// cannot both accept. Production was checked to have no such pair before
	// this was written, so the CREATE cannot fail on history.
	{Version: 30, Name: "audit hash version and fork guard", SQL: `
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS hash_version INTEGER NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX IF NOT EXISTS uq_audit_log_chain ON audit_log (tenant_id, prev_hash);
`},
	// MESHSAT-1366: a tenant owner's consent window for platform support
	// access. pin_hash is Argon2id and is in RedactedInExport. Keyed
	// (tenant_id, id) like every tenant-owned table.
	{Version: 31, Name: "support grants", SQL: `
CREATE TABLE IF NOT EXISTS support_grants (
	tenant_id VARCHAR(64) NOT NULL,
	id VARCHAR(64) NOT NULL,
	pin_hash TEXT NOT NULL,
	created_by_user_id VARCHAR(64) NOT NULL DEFAULT '',
	created_by_email VARCHAR(255) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL,
	used_at TIMESTAMPTZ NULL,
	used_by_email VARCHAR(255) NOT NULL DEFAULT '',
	revoked_at TIMESTAMPTZ NULL,
	failed_attempts INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_support_grants_tenant ON support_grants (tenant_id, expires_at);
`},
	// MESHSAT-1527: a READ-ONLY platform axis on a key.
	//
	// platform_admin (29) cannot be narrowed: RequirePlatformAdmin reads the
	// flag and never the key's role, so a "viewer" key carrying it may still
	// PUT and DELETE tenants, approve sign-ups and requeue receipts. A consumer
	// that only needs to LOOK a customer up (an operator tool answering the
	// MeshSat support line) gets this column instead, and RequirePlatformRead lets
	// it through on GET and HEAD only.
	//
	// DEFAULT false, and the column alone grants nothing: like platform_admin,
	// the handler refuses to SET it unless the caller holds PlatformAdmin.
	{Version: 32, Name: "api key platform read", SQL: `
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS platform_read BOOLEAN NOT NULL DEFAULT false;
`},
	// MESHSAT-1549: Hub federation. A peer is another Hub tenant this tenant's
	// owner added by its certificate; a grant lets one of the peer's modems
	// reach one of this tenant's devices. Both keyed (tenant_id, id); the
	// fingerprint is unique per tenant so a peer cannot be added twice, and a
	// grant is unique per (peer, sender, recipient). relay_url carries the
	// peer's capability token and is in RedactedInExport.
	{Version: 33, Name: "federation peers and grants", SQL: `
CREATE TABLE IF NOT EXISTS federation_peers (
	tenant_id VARCHAR(64) NOT NULL,
	id VARCHAR(64) NOT NULL,
	name VARCHAR(128) NOT NULL DEFAULT '',
	relay_url TEXT NOT NULL,
	cert_pem TEXT NOT NULL,
	fingerprint VARCHAR(64) NOT NULL,
	created_by_email VARCHAR(255) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_federation_peers_fp ON federation_peers (tenant_id, fingerprint);
CREATE TABLE IF NOT EXISTS federation_grants (
	tenant_id VARCHAR(64) NOT NULL,
	id VARCHAR(64) NOT NULL,
	peer_id VARCHAR(64) NOT NULL,
	from_imei VARCHAR(32) NOT NULL,
	to_imei VARCHAR(32) NOT NULL,
	created_by_email VARCHAR(255) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_federation_grants ON federation_grants (tenant_id, peer_id, from_imei, to_imei);
`},
	// MESHSAT-1555: the upstream-compatible Reticulum node's path table,
	// persisted so a leader failover resumes with every known path and the
	// cached announce each path request is answered with. Keyed by the
	// destination hash alone: a Reticulum destination carries no tenant and
	// the node is one transport for every tenant's clients. Nothing in a row
	// is secret (an announce is public by definition).
	{Version: 34, Name: "rns paths", SQL: `
CREATE TABLE IF NOT EXISTS rns_paths (
	dest_hash VARCHAR(32) PRIMARY KEY,
	next_hop VARCHAR(32) NOT NULL,
	hops INTEGER NOT NULL DEFAULT 0,
	iface VARCHAR(64) NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ NOT NULL,
	random_blobs BYTEA NOT NULL DEFAULT ''::bytea,
	announce_raw BYTEA NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`},
	// MESHSAT-1555: a tenant's LXMF inbox and outbox on the Hub's Reticulum
	// node. Keyed (tenant_id, id) like every tenant-owned table; the state
	// index serves the leader's sender, which drains queued rows per tenant.
	// Message text is the tenant's own data and exportable; the identity key
	// lives in the encrypted credentials row, not here.
	{Version: 35, Name: "lxmf messages", SQL: `
CREATE TABLE IF NOT EXISTS lxmf_messages (
	tenant_id VARCHAR(64) NOT NULL,
	id VARCHAR(64) NOT NULL,
	hash VARCHAR(64) NOT NULL DEFAULT '',
	direction VARCHAR(8) NOT NULL,
	state VARCHAR(16) NOT NULL,
	source VARCHAR(32) NOT NULL,
	destination VARCHAR(32) NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	fields_json TEXT NOT NULL DEFAULT '',
	method VARCHAR(16) NOT NULL DEFAULT '',
	stamp_value INTEGER NOT NULL DEFAULT 0,
	signature_valid BOOLEAN NOT NULL DEFAULT true,
	detail TEXT NOT NULL DEFAULT '',
	created_by_email VARCHAR(255) NOT NULL DEFAULT '',
	message_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_lxmf_messages_tenant_created ON lxmf_messages (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_lxmf_messages_tenant_state ON lxmf_messages (tenant_id, state);
`},
	// MESHSAT-1555 P5: what a tenant's LXMF propagation node holds for
	// clients that are offline. The node cannot read a row's data (encrypted
	// to the destination); the destination index serves a client's sync.
	{Version: 36, Name: "lxmf held messages", SQL: `
CREATE TABLE IF NOT EXISTS lxmf_held (
	tenant_id VARCHAR(64) NOT NULL,
	id VARCHAR(64) NOT NULL,
	destination VARCHAR(32) NOT NULL,
	data BYTEA NOT NULL,
	size INTEGER NOT NULL,
	stamp_value INTEGER NOT NULL DEFAULT 0,
	source VARCHAR(32) NOT NULL DEFAULT '',
	received_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_lxmf_held_tenant_dest ON lxmf_held (tenant_id, destination);
CREATE INDEX IF NOT EXISTS idx_lxmf_held_tenant_received ON lxmf_held (tenant_id, received_at);
`},
	// MESHSAT-1584, HeMB-NG stage 2 C4: the Hub app route hub_msg. An operation is keyed
	// (tenant, sender principal, op_id) and is its own outbox entry; msg_mids is the
	// sender's mid index (one content digest per mid); msg_op_events the evidence (C6),
	// published on the sender's msg/status until published_at is set.
	{Version: 37, Name: "hub_msg operations", SQL: `
CREATE TABLE IF NOT EXISTS msg_operations (
	tenant_id VARCHAR(64) NOT NULL,
	sender VARCHAR(160) NOT NULL,
	op_id VARCHAR(36) NOT NULL,
	mid VARCHAR(36) NOT NULL,
	recipient VARCHAR(160) NOT NULL,
	content_type VARCHAR(64) NOT NULL,
	mid_expires_at BIGINT NOT NULL,
	expires_at BIGINT NOT NULL,
	content_sha256 VARCHAR(64) NOT NULL,
	body BYTEA NOT NULL,
	state VARCHAR(16) NOT NULL DEFAULT 'pending',
	conflict BOOLEAN NOT NULL DEFAULT false,
	rev INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	leased_until TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
	recipient_answer VARCHAR(16) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, sender, op_id)
);
CREATE INDEX IF NOT EXISTS idx_msg_operations_mid ON msg_operations (tenant_id, sender, mid);
CREATE INDEX IF NOT EXISTS idx_msg_operations_due ON msg_operations (next_attempt_at) WHERE recipient_answer = '' AND state NOT IN ('completed', 'failed', 'expired');
CREATE INDEX IF NOT EXISTS idx_msg_operations_expires ON msg_operations (expires_at);
CREATE TABLE IF NOT EXISTS msg_mids (
	tenant_id VARCHAR(64) NOT NULL,
	sender VARCHAR(160) NOT NULL,
	mid VARCHAR(36) NOT NULL,
	content_sha256 VARCHAR(64) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, sender, mid)
);
CREATE TABLE IF NOT EXISTS msg_op_events (
	event_id VARCHAR(36) PRIMARY KEY,
	tenant_id VARCHAR(64) NOT NULL,
	sender VARCHAR(160) NOT NULL,
	op_id VARCHAR(36) NOT NULL,
	rev INTEGER NOT NULL,
	class VARCHAR(16) NOT NULL,
	kind VARCHAR(48) NOT NULL,
	reason VARCHAR(64) NOT NULL DEFAULT '',
	authority VARCHAR(16) NOT NULL,
	scope VARCHAR(128) NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	at TIMESTAMPTZ NOT NULL,
	published_at TIMESTAMPTZ NULL,
	UNIQUE (tenant_id, sender, op_id, rev)
);
CREATE INDEX IF NOT EXISTS idx_msg_op_events_unpublished ON msg_op_events (at) WHERE published_at IS NULL;
`},
	// MESHSAT-1584, HeMB-NG stage 2 C10: one record per SOS a sender raised under its own
	// identity, keyed (tenant, origin principal, sos_id). It pages once whichever bearers
	// carry the SOS, and a cancel is terminal, also when it arrives before the start.
	{Version: 38, Name: "sos runs", SQL: `
CREATE TABLE IF NOT EXISTS sos_runs (
	tenant_id VARCHAR(64) NOT NULL,
	origin VARCHAR(160) NOT NULL,
	sos_id VARCHAR(36) NOT NULL,
	bridge_id VARCHAR(128) NOT NULL DEFAULT '',
	run_start_ms BIGINT NOT NULL,
	state VARCHAR(16) NOT NULL,
	rev INTEGER NOT NULL DEFAULT 0,
	bearers VARCHAR(256) NOT NULL DEFAULT '',
	device_id VARCHAR(64) NOT NULL DEFAULT '',
	text TEXT NOT NULL DEFAULT '',
	lat DOUBLE PRECISION NOT NULL DEFAULT 0,
	lon DOUBLE PRECISION NOT NULL DEFAULT 0,
	has_position BOOLEAN NOT NULL DEFAULT false,
	alert_id VARCHAR(200) NOT NULL DEFAULT '',
	started_at TIMESTAMPTZ NULL,
	cancelled_at TIMESTAMPTZ NULL,
	cancelled_by VARCHAR(160) NOT NULL DEFAULT '',
	cancel_reason INTEGER NOT NULL DEFAULT -1,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, origin, sos_id)
);
CREATE INDEX IF NOT EXISTS idx_sos_runs_created ON sos_runs (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sos_runs_sos_id ON sos_runs (tenant_id, sos_id);
`},
	// MESHSAT-1584 C10 (r16 review C10-R2): the alert work a run still owes, set with its
	// state change and cleared once done; the reconciler lists the owed ones.
	{Version: 39, Name: "sos runs alert pending", SQL: `
ALTER TABLE sos_runs ADD COLUMN IF NOT EXISTS alert_pending BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_sos_runs_alert_pending ON sos_runs (updated_at) WHERE alert_pending;
`},
	// MESHSAT-1584, HeMB-NG stage 2 C7: the tenant directory (contacts, their addresses and
	// keys, groups, dispatch policies, the per-tenant version) on Postgres, until now on
	// SQLite only. Every table carries tenant_id and every key starts with it, so an id or an
	// address is unique within its tenant only and no upsert can move a row between tenants.
	// The foreign keys all cascade inside one tenant, so a tenant purge may delete these
	// tables in any order. seeded marks the precedence defaults the Hub installed itself.
	// The statements live in internal/directory (shared with the SQLite schema there).
	{Version: 40, Name: "directory", SQL: `
CREATE TABLE IF NOT EXISTS directory_contacts (
	tenant_id VARCHAR(64) NOT NULL,
	id TEXT NOT NULL,
	display_name TEXT NOT NULL,
	given_name TEXT NOT NULL DEFAULT '',
	family_name TEXT NOT NULL DEFAULT '',
	org TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT '',
	team TEXT NOT NULL DEFAULT '',
	sidc TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	trust_level INTEGER NOT NULL DEFAULT 0,
	trust_verified_at TIMESTAMPTZ NULL,
	origin TEXT NOT NULL DEFAULT 'hub',
	policy_id TEXT NOT NULL DEFAULT '',
	hub_version BIGINT NOT NULL DEFAULT 0,
	hub_etag TEXT NOT NULL DEFAULT '',
	metadata TEXT NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_dir_contacts_name ON directory_contacts (tenant_id, display_name);
CREATE TABLE IF NOT EXISTS directory_addresses (
	tenant_id VARCHAR(64) NOT NULL,
	id TEXT NOT NULL,
	contact_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	value TEXT NOT NULL,
	subvalue TEXT NOT NULL DEFAULT '',
	label TEXT NOT NULL DEFAULT '',
	primary_rank INTEGER NOT NULL DEFAULT 0,
	verified BOOLEAN NOT NULL DEFAULT false,
	bearer_hint INTEGER NOT NULL DEFAULT 50,
	max_cost_cents INTEGER NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id),
	UNIQUE (tenant_id, kind, value),
	FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_dir_addr_contact ON directory_addresses (tenant_id, contact_id);
CREATE TABLE IF NOT EXISTS directory_contact_keys (
	tenant_id VARCHAR(64) NOT NULL,
	id TEXT NOT NULL,
	contact_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 1,
	status TEXT NOT NULL DEFAULT 'active',
	public_data BYTEA NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	algorithm TEXT NOT NULL DEFAULT '',
	trust_anchor TEXT NOT NULL DEFAULT 'hub',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id),
	UNIQUE (tenant_id, contact_id, kind, version),
	FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS directory_groups (
	tenant_id VARCHAR(64) NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'LIST',
	sidc TEXT NOT NULL DEFAULT '',
	mls_group_id TEXT NOT NULL DEFAULT '',
	policy_id TEXT NOT NULL DEFAULT '',
	hub_version BIGINT NOT NULL DEFAULT 0,
	hub_etag TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE TABLE IF NOT EXISTS directory_group_members (
	tenant_id VARCHAR(64) NOT NULL,
	group_id TEXT NOT NULL,
	contact_id TEXT NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	role TEXT NOT NULL DEFAULT '',
	added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, group_id, contact_id),
	FOREIGN KEY (tenant_id, group_id) REFERENCES directory_groups (tenant_id, id) ON DELETE CASCADE,
	FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_dir_members_contact ON directory_group_members (tenant_id, contact_id);
CREATE TABLE IF NOT EXISTS directory_dispatch_policy (
	tenant_id VARCHAR(64) NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	scope_type TEXT NOT NULL DEFAULT 'contact',
	scope_id TEXT NOT NULL DEFAULT '',
	strategy TEXT NOT NULL DEFAULT 'PRIMARY_ONLY',
	preferred TEXT NOT NULL DEFAULT '[]',
	fallback TEXT NOT NULL DEFAULT '[]',
	require_encryption BOOLEAN NOT NULL DEFAULT false,
	max_retries INTEGER NOT NULL DEFAULT 0,
	retry_delay_ns BIGINT NOT NULL DEFAULT 0,
	seeded BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id),
	UNIQUE (tenant_id, scope_type, scope_id)
);
CREATE TABLE IF NOT EXISTS directory_versions (
	tenant_id VARCHAR(64) PRIMARY KEY,
	version BIGINT NOT NULL DEFAULT 0,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`},
	// MESHSAT-1584 C7.6: the directory signing key's rotation records, one per tenant per
	// rotation, each signed by the key it rotates away from and holding a slot in the
	// tenant's version sequence. Kept for good: a bridge that was offline across several
	// rotations walks the chain from the key it pinned.
	{Version: 41, Name: "directory rotations", SQL: `
CREATE TABLE IF NOT EXISTS directory_rotations (
	tenant_id VARCHAR(64) NOT NULL,
	version BIGINT NOT NULL,
	old_key_id VARCHAR(64) NOT NULL,
	new_key_id VARCHAR(64) NOT NULL,
	envelope TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, version),
	UNIQUE (tenant_id, old_key_id)
);
`},
	// MESHSAT-1584 (HeMB-NG S3-2, r28): the last satellite payload format a
	// bridge declared for a modem ("sealed" or "plain"), kept per modem so a
	// birth that lacks the modem's IMEI or the gateway does not undo it.
	{Version: 42, Name: "device egress declaration", SQL: `
ALTER TABLE devices ADD COLUMN IF NOT EXISTS egress_format VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS egress_bridge_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN IF NOT EXISTS egress_declared_at TIMESTAMPTZ NULL;
`},
	// MESHSAT-1584 (HeMB-NG S3-2, r29): what each bridge declared per modem
	// (IMEI to "sealed" or "plain"), merged in the statement that saves its
	// birth, so a failed per-device write cannot leave the two disagreeing.
	{Version: 43, Name: "bridge egress declarations", SQL: `
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS egress_declarations JSONB NOT NULL DEFAULT '{}'::jsonb;
`},
	// MESHSAT-1584 (r30): births ordered by their own timestamp, modem links
	// by when the Hub received the birth, and the declarations per bridge and
	// modem with the stamp of the birth that made them, so a birth written
	// again later (a retry, or another replica behind) never undoes a newer
	// one. The r29 declarations move over with stamp 0, older than any birth.
	{Version: 44, Name: "ordered births and declarations", SQL: `
ALTER TABLE bridges ADD COLUMN IF NOT EXISTS birth_stamp BIGINT NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS bridge_linked_at BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS bridge_egress (
	tenant_id VARCHAR(64) NOT NULL,
	bridge_id VARCHAR(64) NOT NULL,
	imei VARCHAR(64) NOT NULL,
	format VARCHAR(16) NOT NULL,
	stamp BIGINT NOT NULL DEFAULT 0,
	declared_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, bridge_id, imei)
);
INSERT INTO bridge_egress (tenant_id, bridge_id, imei, format, stamp)
	SELECT b.tenant_id, b.bridge_id, e.key, e.value, 0
	FROM bridges b, jsonb_each_text(b.egress_declarations) e
	WHERE e.value IN ('sealed', 'plain')
	ON CONFLICT DO NOTHING;
UPDATE bridges SET egress_declarations = '{}'::jsonb WHERE egress_declarations <> '{}'::jsonb;
`},
	// MESHSAT-1685: the "@" lane. A device's conversation with one person
	// (store.PersonConversation) and the ledger of what the lane sent, so the
	// replies a person can make the Hub send have a ceiling that holds across
	// both replicas. The ref is unique per tenant: an email reply names its
	// conversation by it. The ledger never holds a body. tenants gains the
	// tenant's choice of who its devices may write to (store.Tenant). The
	// buckets are the ceilings: one row per (tenant, window), counted up in
	// one statement that refuses past the limit, so a check is one write
	// however far over the limit a sender is.
	{Version: 45, Name: "person lane", SQL: `
CREATE TABLE IF NOT EXISTS person_conversations (
	tenant_id           VARCHAR(64) NOT NULL,
	id                  VARCHAR(64) NOT NULL,
	device_id           VARCHAR(64) NOT NULL,
	device_kind         VARCHAR(16) NOT NULL,
	peer_kind           VARCHAR(16) NOT NULL,
	peer                VARCHAR(320) NOT NULL,
	tag                 VARCHAR(8) NOT NULL,
	ref                 VARCHAR(32) NOT NULL,
	last_out_at         TIMESTAMPTZ NOT NULL,
	last_in_at          TIMESTAMPTZ NULL,
	last_out_message_id VARCHAR(255) NOT NULL DEFAULT '',
	last_in_message_id  VARCHAR(255) NOT NULL DEFAULT '',
	created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id),
	UNIQUE (tenant_id, ref)
);
CREATE INDEX IF NOT EXISTS idx_person_conversations_peer
	ON person_conversations (tenant_id, peer_kind, peer, last_out_at);
CREATE TABLE IF NOT EXISTS person_chat_sends (
	tenant_id       VARCHAR(64) NOT NULL,
	id              VARCHAR(96) NOT NULL,
	conversation_id VARCHAR(64) NOT NULL,
	direction       VARCHAR(16) NOT NULL,
	e2e             BOOLEAN NOT NULL DEFAULT false,
	bytes           INTEGER NOT NULL DEFAULT 0,
	status          VARCHAR(32) NOT NULL DEFAULT '',
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_person_chat_sends_conv
	ON person_chat_sends (tenant_id, conversation_id, direction, created_at);
CREATE INDEX IF NOT EXISTS idx_person_chat_sends_created
	ON person_chat_sends (tenant_id, direction, created_at);
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS person_chat_mode VARCHAR(16) NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS person_chat_buckets (
	tenant_id  VARCHAR(64) NOT NULL,
	key        VARCHAR(200) NOT NULL,
	n          INTEGER NOT NULL DEFAULT 0,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, key)
);
CREATE INDEX IF NOT EXISTS idx_person_chat_buckets_age ON person_chat_buckets (tenant_id, updated_at);
`},
	// MESHSAT-1706, TAK direct messages. tak_contacts is the crews the tenant's
	// clients put on the TAK map, keyed (tenant, uid), with the bridge whose
	// export last carried each: a chat addressed to the crew goes to that bridge
	// alone. The leader writes it behind its own memory (takhosted). The
	// messages are what the tenant's operators wrote to a crew from the Hub's map
	// and what the crew answered, keyed (tenant, GeoChat message id) so both
	// replicas storing one answer, or a resent request, is one row; kept 30 days.
	{Version: 46, Name: "tak direct messages", SQL: `
CREATE TABLE IF NOT EXISTS tak_contacts (
	tenant_id    VARCHAR(64) NOT NULL,
	uid          VARCHAR(255) NOT NULL,
	callsign     VARCHAR(255) NOT NULL DEFAULT '',
	bridge_id    VARCHAR(128) NOT NULL,
	last_seen_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, uid)
);
CREATE INDEX IF NOT EXISTS idx_tak_contacts_seen ON tak_contacts (tenant_id, last_seen_at);
CREATE TABLE IF NOT EXISTS tak_chat_messages (
	tenant_id    VARCHAR(64) NOT NULL,
	id           VARCHAR(96) NOT NULL,
	crew_uid     VARCHAR(255) NOT NULL,
	operator_uid VARCHAR(128) NOT NULL,
	user_id      VARCHAR(128) NOT NULL DEFAULT '',
	direction    VARCHAR(8) NOT NULL,
	text         TEXT NOT NULL,
	bridge_id    VARCHAR(128) NOT NULL DEFAULT '',
	status       VARCHAR(32) NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_tak_chat_messages_crew ON tak_chat_messages (tenant_id, crew_uid, created_at);
CREATE INDEX IF NOT EXISTS idx_tak_chat_messages_age ON tak_chat_messages (tenant_id, created_at);
`},
	// MESHSAT-1736, the Matrix bridge. matrix_operators maps the operator id a
	// Matrix user writes to a crew under back to the room and the user, so the
	// crew's answer is posted where the question was asked; kept 30 days after
	// last use. matrix_sync is the tenant bot's next_batch, so a new leader
	// resumes where the old one stopped.
	{Version: 47, Name: "matrix bridge", SQL: `
CREATE TABLE IF NOT EXISTS matrix_operators (
	tenant_id      VARCHAR(64) NOT NULL,
	user_id        VARCHAR(64) NOT NULL,
	room_id        VARCHAR(255) NOT NULL,
	matrix_user_id VARCHAR(255) NOT NULL,
	display_name   VARCHAR(255) NOT NULL DEFAULT '',
	last_used_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_matrix_operators_used ON matrix_operators (tenant_id, last_used_at);
CREATE TABLE IF NOT EXISTS matrix_sync (
	tenant_id   VARCHAR(64) NOT NULL PRIMARY KEY,
	bot_user_id VARCHAR(255) NOT NULL DEFAULT '',
	next_batch  TEXT NOT NULL DEFAULT '',
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`},
	// MESHSAT-1735, voice notes and photos: the audio and image fields an
	// LXMF message in a tenant's inbox or outbox carried. One row per
	// attachment, keyed by the message and its index in it (the image
	// first); kind audio or image, mode the LXMF audio mode, format the
	// image format, size the bytes, which live in the object store under
	// object_key. A tenant keeps at most HUB_LXMF_ATTACHMENT_MAX_BYTES of
	// them, oldest out first.
	{Version: 48, Name: "lxmf attachments", SQL: `
CREATE TABLE IF NOT EXISTS lxmf_attachments (
	tenant_id  VARCHAR(64) NOT NULL,
	message_id VARCHAR(64) NOT NULL,
	idx        INTEGER NOT NULL,
	kind       VARCHAR(16) NOT NULL,
	mode       INTEGER NOT NULL DEFAULT 0,
	format     VARCHAR(16) NOT NULL DEFAULT '',
	size       INTEGER NOT NULL,
	object_key VARCHAR(255) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, message_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_lxmf_attachments_age ON lxmf_attachments (tenant_id, created_at);
`},
	// MESHSAT-1733, the Community Post. A device's profile: "" for an
	// ordinary modem, "post" for a kit that runs the Community Post, whose
	// "@" lane texts come from many people at one modem and carry a pickup
	// code ("[#K7QW2X]") the lane keys each conversation by, so two people
	// writing to the same number never share one. Mirrored in sqlite.
	{Version: 49, Name: "device profile", SQL: `
ALTER TABLE devices ADD COLUMN IF NOT EXISTS profile VARCHAR(16) NOT NULL DEFAULT '';
`},
	// MESHSAT-1734, the last-mile warning relay. warnings holds every CAP
	// message a feed carried, keyed (source, identifier); it has no tenant
	// column on purpose, a public warning names an area. warning_matches is
	// the tenant's side: which warnings matched its areas or kits and went
	// to which bridges.
	{Version: 50, Name: "warning relay", SQL: `
CREATE TABLE IF NOT EXISTS warnings (
	source      VARCHAR(32) NOT NULL,
	identifier  VARCHAR(255) NOT NULL,
	sender      VARCHAR(255) NOT NULL DEFAULT '',
	sender_name VARCHAR(255) NOT NULL DEFAULT '',
	msg_type    VARCHAR(16) NOT NULL,
	status      VARCHAR(16) NOT NULL DEFAULT '',
	refs        TEXT NOT NULL DEFAULT '',
	sent        TIMESTAMPTZ NOT NULL,
	effective   TIMESTAMPTZ NULL,
	expires     TIMESTAMPTZ NULL,
	severity    VARCHAR(16) NOT NULL DEFAULT '',
	urgency     VARCHAR(16) NOT NULL DEFAULT '',
	certainty   VARCHAR(16) NOT NULL DEFAULT '',
	category    VARCHAR(16) NOT NULL DEFAULT '',
	event       VARCHAR(255) NOT NULL DEFAULT '',
	headline    TEXT NOT NULL DEFAULT '',
	language    VARCHAR(16) NOT NULL DEFAULT '',
	areas       TEXT NOT NULL DEFAULT '[]',
	text        TEXT NOT NULL DEFAULT '',
	raw_hash    VARCHAR(64) NOT NULL DEFAULT '',
	first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (source, identifier)
);
CREATE INDEX IF NOT EXISTS idx_warnings_seen ON warnings (last_seen);
CREATE TABLE IF NOT EXISTS warning_matches (
	tenant_id   VARCHAR(64) NOT NULL,
	source      VARCHAR(32) NOT NULL,
	identifier  VARCHAR(255) NOT NULL,
	msg_type    VARCHAR(16) NOT NULL,
	test        BOOLEAN NOT NULL DEFAULT false,
	bridges     TEXT NOT NULL DEFAULT '[]',
	detail      TEXT NOT NULL DEFAULT '',
	matched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, source, identifier)
);
CREATE INDEX IF NOT EXISTS idx_warning_matches_age ON warning_matches (tenant_id, matched_at);
`},
}
