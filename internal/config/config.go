package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the MeshSat Hub service.
type Config struct {
	// Tri-mode: "standalone" (default), "cluster", "kubernetes"
	Mode        string `yaml:"mode"`
	DatabaseURL string `yaml:"database_url"` // MariaDB or Postgres DSN (cluster/k8s)
	DBDriver    string `yaml:"db_driver"`    // "sqlite" or "postgres"; empty = sniffed from DatabaseURL
	SQLitePath  string `yaml:"sqlite_path"`  // SQLite database file (default /data/hub.db)
	RedisURL    string `yaml:"redis_url"`    // Redis URL (cluster/k8s only)
	NATSUrl     string `yaml:"nats_url"`     // External NATS URL (cluster/k8s only)

	Port                    int    `yaml:"port"`
	MQTTBrokerURL           string `yaml:"mqtt_broker_url"`
	MQTTClientID            string `yaml:"mqtt_client_id"`
	MQTTTLSCert             string `yaml:"mqtt_tls_cert"` // Client certificate PEM for mutual TLS
	MQTTTLSKey              string `yaml:"mqtt_tls_key"`  // Client private key PEM
	MQTTLSCA                string `yaml:"mqtt_tls_ca"`   // CA certificate for broker verification
	RockBLOCKSecret         string `yaml:"rockblock_secret"`
	CloudloopAPIKey         string `yaml:"cloudloop_api_key"`
	CloudloopAPIURL         string `yaml:"cloudloop_api_url"`
	GlobalstarAPIKey        string `yaml:"globalstar_api_key"`
	GlobalstarAPIURL        string `yaml:"globalstar_api_url"`
	GlobalstarWebhookSecret string `yaml:"globalstar_webhook_secret"` // HMAC-SHA256 secret for Globalstar MO webhook verification
	LogLevel                string `yaml:"log_level"`
	LogFormat               string `yaml:"log_format"`
	AuthToken               string `yaml:"auth_token"`
	AuthMode                string `yaml:"auth_mode"`       // "none", "token", "local", "oidc"
	JWTSigningKey           string `yaml:"jwt_signing_key"` // HMAC-SHA256 key for local auth JWT (min 32 chars)
	OIDCIssuerURL           string `yaml:"oidc_issuer_url"`
	OIDCAudience            string `yaml:"oidc_audience"`
	OIDCClientID            string `yaml:"oidc_client_id"`             // authorization-code flow client id (mode=oidc)
	OIDCClientSecret        string `yaml:"oidc_client_secret"`         // confidential client secret
	OIDCRedirectURI         string `yaml:"oidc_redirect_uri"`          // e.g. https://hub.meshsat.net/api/auth/oidc/callback
	OIDCScopes              string `yaml:"oidc_scopes"`                // default "openid profile email"
	OIDCGroupsClaim         string `yaml:"oidc_groups_claim"`          // claim carrying group names (default "groups")
	OIDCAdminGroup          string `yaml:"oidc_admin_group"`           // group granting platform admin (default meshsat-platform-admin)
	OIDCBootstrapOwnerEmail string `yaml:"oidc_bootstrap_owner_email"` // this account attaches to the default tenant instead of creating one
	OIDCSignupURL           string `yaml:"oidc_signup_url"`            // "Request beta access" link on the login page (authentik enrollment flow)
	// Approving a beta request from inside the Hub (MESHSAT-978). Without a
	// token the endpoints report themselves unconfigured and the manual
	// script stays the way to do it.
	AuthentikURL      string `yaml:"authentik_url"`
	AuthentikToken    string `yaml:"authentik_token"`
	CommunityURL      string `yaml:"community_url"`       // MeshSat community room (Matrix) shown while an account awaits approval
	LocalLoginEnabled *bool  `yaml:"local_login_enabled"` // email/password login; default true in local mode, false in oidc mode
	MetricsToken      string `yaml:"metrics_token"`       // when set, /metrics requires this bearer token

	// TAK is per-tenant (MESHSAT-1037, MESHSAT-1065): each tenant gets its own
	// OpenTAKServer, or points the Hub at a server they run. The platform-wide
	// settings that were here -- one TAK host and port, the OTS REST poller and its
	// marker ceiling -- went with the code that read them (MESHSAT-1032). The
	// hosted front's own settings are further down (TAKFront*, TAKNamespace), and a
	// tenant's own server is a provider account, not config.

	// PlanDeviceLimits overrides a subscription tier's combined device and
	// bridge ceiling, so a tier can be re-priced without a deploy:
	// HUB_PLAN_FREE_DEVICES, HUB_PLAN_CREW_DEVICES, HUB_PLAN_FLEET_DEVICES,
	// HUB_PLAN_CUSTOM_DEVICES. -1 means no ceiling. A tier absent here keeps
	// the built-in default (internal/plans).
	PlanDeviceLimits map[string]int `yaml:"plan_device_limits"`

	// PlanTAKUserLimits overrides a tier's ceiling on TAK accounts in the
	// tenant's own hosted OpenTAKServer (MESHSAT-1037):
	// HUB_PLAN_FREE_TAK_USERS, HUB_PLAN_CREW_TAK_USERS,
	// HUB_PLAN_FLEET_TAK_USERS, HUB_PLAN_CUSTOM_TAK_USERS. -1 means no
	// ceiling; a tier absent here keeps the built-in default (internal/plans).
	//
	// A separate knob from PlanDeviceLimits because it meters something else: a
	// TAK user is a person with a certificate, a device is a piece of kit, and
	// a small fleet can have many people watching the map.
	PlanTAKUserLimits map[string]int `yaml:"plan_tak_user_limits"`

	// The hosted TAK front (MESHSAT-1037): one TLS listener serving every
	// tenant's phones, identifying the tenant from the client certificate's
	// issuer and proxying into that tenant's own OpenTAKServer.
	//
	// TAKFrontEnabled is the switch. It stays off until the certificate below
	// exists, because the front cannot be started without one.
	TAKFrontEnabled bool `yaml:"tak_front_enabled"`
	// TAKFrontAddr is what the listener binds, default :8089 -- the standard TAK
	// SSL port, matching the public one so the edge configuration needs no
	// mental translation.
	TAKFrontAddr string `yaml:"tak_front_addr"`
	// TAKFrontCertFile and TAKFrontKeyFile are the ONE server certificate every
	// phone sees.
	//
	// It cannot be per tenant: ATAK sends no SNI on the CoT socket, so the front
	// must present a certificate before it knows which tenant is calling. Every
	// tenant's truststore therefore carries this certificate's chain, which is
	// also why it is supplied rather than generated -- a certificate the Hub
	// minted for itself would have to be distributed to every phone that already
	// trusts the previous one.
	TAKFrontCertFile string `yaml:"tak_front_cert_file"`
	TAKFrontKeyFile  string `yaml:"tak_front_key_file"`
	// TAKNamespace holds the custom resources. The Hub runs in meshsat-hub and
	// reaches across into this one, which is why its Role for them is a separate
	// Role in that namespace.
	TAKNamespace string `yaml:"tak_namespace"`

	// Stripe: the payment provider (MESHSAT-1023).
	//
	// Three separate secrets, and conflating any two of them is a real mistake:
	//
	//   StripeSecretKey     calls the API. Never appears in a URL.
	//   StripeWebhookSecret verifies a delivery's signature. This is the only
	//                       thing that authenticates a caller, and it must
	//                       never go in the path either -- a URL travels
	//                       through consoles, logs and support tickets.
	//   StripePathSecret    forms the webhook URL. It identifies the endpoint
	//                       and keeps it off scanners; it authenticates
	//                       nothing.
	//
	// With the key or the signing secret empty the endpoint refuses everything,
	// which is the right state for a payment endpoint nobody configured.
	StripeSecretKey     string `yaml:"stripe_secret_key"`
	StripeWebhookSecret string `yaml:"stripe_webhook_secret"`
	StripePathSecret    string `yaml:"stripe_path_secret"`

	// StripePublishableKey is the ONE Stripe value meant to be public: it is
	// served to the browser so Stripe.js can mount the embedded donation form,
	// and on its own it can neither create nor read anything. It went unused
	// while the Hub used only hosted Checkout, where nothing ran client-side.
	// Read through stripeSecret anyway -- a missing key renders the literal
	// "<no value>" and a page built on that is broken rather than absent.
	StripePublishableKey string `yaml:"stripe_publishable_key"`
	// StripePrices maps a Stripe price id to a plan. A price that names
	// anything but a sellable tier is refused at load: custom and beta are
	// unlimited and operator-set.
	StripePrices map[string]string `yaml:"stripe_prices"`
	// StripeDonationPrice is a price with a customer-chosen amount. Empty means
	// no donation path, which is a supported state.
	StripeDonationPrice string `yaml:"stripe_donation_price"`
	// StripeTimeout bounds a call to the API.
	StripeTimeout time.Duration `yaml:"stripe_timeout"`

	// Invoice Ninja: the billing system that issues customer receipts
	// (MESHSAT-998). Empty URL or token means no receipts are issued at all --
	// payments still grant plans, and the outbox rows accumulate unsent, which
	// is recoverable. Issuing them into the wrong company would not be.
	//
	// The token is COMPANY-SCOPED: each Invoice Ninja company has its own, and
	// it is the token that decides which brand, VAT treatment and number
	// series a receipt is issued under. Pointing this at the wrong one puts
	// consumer receipts in a business's invoice sequence.
	InvoiceNinjaURL   string `yaml:"invoiceninja_url"`
	InvoiceNinjaToken string `yaml:"invoiceninja_token"`
	// InvoiceNinjaTaxName and InvoiceNinjaTaxRate go on every invoice line,
	// because the company default tax is a web-UI prefill the API does not
	// apply. The company has inclusive taxes on, so this is derived OUT of the
	// price the customer paid, never added to it.
	InvoiceNinjaTaxName string  `yaml:"invoiceninja_tax_name"`
	InvoiceNinjaTaxRate float64 `yaml:"invoiceninja_tax_rate"`
	// InvoiceNinjaCurrency is the only currency that company invoices in. A
	// payment in anything else is parked for a person rather than converted.
	InvoiceNinjaCurrency string `yaml:"invoiceninja_currency"`
	// InvoiceNinjaCountryID is the numeric country a new customer record is
	// created with (528 = the Netherlands).
	InvoiceNinjaCountryID string `yaml:"invoiceninja_country_id"`
	// InvoiceNinjaTimeout bounds one call to the billing system.
	InvoiceNinjaTimeout time.Duration `yaml:"invoiceninja_timeout"`

	// OIDCRecoveryURL is the identity provider's password reset flow, linked
	// from the login page. It has existed and been bound to the MeshSat brand
	// since MESHSAT-978, but nothing in the Hub pointed at it, so a user who
	// forgot their password had no route to recovery from the login screen.
	OIDCRecoveryURL string `yaml:"oidc_recovery_url"`

	// OIDCPasswordChangeURL and OIDCMFASetupURL are the identity provider's
	// self-service flows for a signed-in customer. They need naming here because
	// authentik's own settings page (/if/user/) refuses `external` users -- which
	// every customer is, by licensing -- so a customer could neither change their
	// password nor turn on two-factor, while the sign-in flow had been validating
	// MFA all along. A flow executor is not that settings page and works for them.
	OIDCPasswordChangeURL string `yaml:"oidc_password_change_url"`
	OIDCMFASetupURL       string `yaml:"oidc_mfa_setup_url"`

	// AuthRateLimitPerMin bounds the unauthenticated auth endpoints per client
	// IP per minute: /api/auth/config, the OIDC login and callback pair, and
	// refresh. The callback performs an outbound token exchange against
	// authentik on every call, so this is what stops an anonymous caller
	// aiming our fan-out at our own identity provider.
	AuthRateLimitPerMin int `yaml:"auth_rate_limit_per_min"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For we believe, used to
	// resolve the real client IP for rate limiting. Empty means trust none and
	// key on the direct peer -- safe, but it groups every client behind the
	// ingress into one bucket.
	TrustedProxies string `yaml:"trusted_proxies"`

	// UpgradeURL is where a tenant goes to pay for a larger tier. Shown beside
	// the tier table in the app; empty hides the link rather than guessing.
	UpgradeURL string `yaml:"upgrade_url"`

	// Transactional email. The Hub sends a handful of messages a customer must
	// receive because something happened to their account: approved, plan
	// changed, plan about to lapse, plan lapsed. Receipts are NOT sent from
	// here -- Invoice Ninja issues those from its own outbox.
	//
	// SMTPRelay is host:port of an IP-authorised relay that signs outbound mail
	// with DKIM; no credentials, which is how Invoice Ninja submits from the
	// same estate. Empty means the Hub sends nothing and says so at boot.
	SMTPRelay    string        `yaml:"smtp_relay"`
	MailFrom     string        `yaml:"mail_from"`
	MailFromName string        `yaml:"mail_from_name"`
	MailTimeout  time.Duration `yaml:"mail_timeout"`
	// PublicURL is where a customer signs in. It appears in the mail above, so
	// it has to be the address they can actually reach, not an internal one.
	PublicURL string `yaml:"public_url"`

	// APRS-IS IGate
	APRSISEnabled  bool   `yaml:"aprsis_enabled"`
	APRSISServer   string `yaml:"aprsis_server"`
	APRSISCallsign string `yaml:"aprsis_callsign"`
	APRSISPasscode string `yaml:"aprsis_passcode"`

	// Per-device rate limiting for MT sends
	RateLimitBurst        int     `yaml:"ratelimit_burst"`          // max burst tokens per device (default 10)
	RateLimitRefillPerMin float64 `yaml:"ratelimit_refill_per_min"` // tokens refilled per minute (default 1)
	RateLimitDailyCap     int     `yaml:"ratelimit_daily_cap"`      // max sends per device per day (default 100, 0=unlimited)
	RateLimitMonthlyCap   int     `yaml:"ratelimit_monthly_cap"`    // max sends per device per month (default 0=unlimited)

	// Tenant isolation
	TenantEnforce bool `yaml:"tenant_enforce"` // If true, requests without tenant context get 403

	// Apprise notifications
	AppriseEnabled bool   `yaml:"apprise_enabled"`
	AppriseURL     string `yaml:"apprise_url"` // Apprise API base URL (e.g., http://apprise:8000)

	// ntfy push notifications
	NtfyEnabled bool   `yaml:"ntfy_enabled"`
	NtfyURL     string `yaml:"ntfy_url"`   // ntfy server URL (e.g., https://ntfy.sh or http://ntfy:80)
	NtfyToken   string `yaml:"ntfy_token"` // optional access token for protected topics

	// Cluster peers (comma-separated Hub URLs for cluster-wide health view)
	ClusterPeers string `yaml:"cluster_peers"` // e.g., "https://192.168.15.10:8451"

	// hawkBit OTA
	HawkBitEnabled  bool   `yaml:"hawkbit_enabled"`
	HawkBitURL      string `yaml:"hawkbit_url"`      // hawkBit Management API URL (e.g., http://hawkbit:8080)
	HawkBitUsername string `yaml:"hawkbit_username"` // Management API username
	HawkBitPassword string `yaml:"hawkbit_password"` // Management API password

	// TLS certificate pinning (base64-encoded SHA-256 SPKI hashes)
	TLSPinPrimary string `yaml:"tls_pin_primary"` // Primary pin hash
	TLSPinBackup  string `yaml:"tls_pin_backup"`  // Backup pin for rotation

	// OIDC provider certificate pinning (base64-encoded SHA-256 SPKI hashes)
	OIDCCertPin       string `yaml:"oidc_cert_pin"`        // Primary OIDC provider cert pin
	OIDCCertPinBackup string `yaml:"oidc_cert_pin_backup"` // Backup pin for rotation

	// Rock7 RockBLOCK MT API (for sending messages to devices via Iridium)
	Rock7Username string `yaml:"rock7_username"` // Rock7 web services username
	Rock7Password string `yaml:"rock7_password"` // Rock7 web services password

	// SOS detection
	SOSChainID string `yaml:"sos_chain_id"` // Default escalation chain ID for SOS alerts (empty = first available)

	// Email gateway (SMTP + PGP)
	EmailEnabled  bool   `yaml:"email_enabled"`
	EmailSMTPHost string `yaml:"email_smtp_host"` // host:port (e.g., smtp.example.com:587)
	EmailFrom     string `yaml:"email_from"`      // sender address
	EmailUsername string `yaml:"email_username"`  // SMTP auth username
	EmailPassword string `yaml:"email_password"`  // SMTP auth password
	EmailPGPKey   string `yaml:"email_pgp_key"`   // Hub PGP private key (armored) — empty = generate on start
	// Out-of-band bridge commands over SMS / Iridium MT (MESHSAT-964).
	OOBEncrypt    bool          `yaml:"oob_encrypt"`      // seal frame args encrypted (default true)
	OOBMaxPerHour int           `yaml:"oob_max_per_hour"` // outbound frames per bridge per bearer per hour (default 20)
	OOBSMSTimeout time.Duration `yaml:"oob_sms_timeout"`  // reply wait over SMS (default 60s)
	OOBSatTimeout time.Duration `yaml:"oob_sat_timeout"`  // reply wait over Iridium (default 10m)

	// EmailWebhookSecret gates POST /api/webhook/email (X-Webhook-Secret or ?secret=); unset = webhook refused.
	EmailWebhookSecret string `yaml:"email_webhook_secret"`

	// SMS gateway (Twilio)
	SMSEnabled       bool   `yaml:"sms_enabled"`
	SMSAccountSID    string `yaml:"sms_account_sid"`    // Twilio Account SID (AC...)
	SMSAuthToken     string `yaml:"sms_auth_token"`     // Twilio Auth Token (or API Key Secret)
	SMSAPIKeySID     string `yaml:"sms_api_key_sid"`    // Twilio API Key SID (SK...) — if set, uses API key auth
	SMSFromNumber    string `yaml:"sms_from_number"`    // E.164 sender number
	SMSWebhookSecret string `yaml:"sms_webhook_secret"` // HMAC secret for inbound webhook verification

	// Reticulum identity
	ReticulumIdentityFile string `yaml:"reticulum_identity_file"` // Path to persist Hub's Reticulum identity keypair (default: data/reticulum_identity.json)
	ReticulumAppName      string `yaml:"reticulum_app_name"`      // Reticulum destination app name (default: meshsat.hub)

	// Reticulum TCP interface (HDLC framing for external RNS node connectivity)
	ReticulumTCPEnabled bool   `yaml:"reticulum_tcp_enabled"` // Enable TCP listener for RNS nodes (default true)
	ReticulumTCPAddr    string `yaml:"reticulum_tcp_addr"`    // TCP listen address (default ":4242")

	// Cloudloop MO webhook + MQTT subscriber
	CloudloopWebhookAllowedIPs string `yaml:"cloudloop_webhook_allowed_ips"` // comma-separated IP allowlist (default: Cloudloop IPs)

	// CloudloopWebhookExpectedIPs is OBSERVE-ONLY and never refuses a delivery.
	// The allowlist above is "*" because Cloudloop publishes no egress addresses
	// to allowlist, and narrowing it by guesswork would risk dropping a
	// satellite MO message -- the path an SOS arrives on. This records where
	// deliveries actually come from instead, which is what would show the path
	// secret had leaked. Accepts single addresses and CIDRs.
	CloudloopWebhookExpectedIPs string `yaml:"cloudloop_webhook_expected_ips"`
	CloudloopWebhookToken       string `yaml:"cloudloop_webhook_token"` // shared token (?token= or X-Webhook-Token); required when the allowlist is "*"
	CloudloopMQTTBroker         string `yaml:"cloudloop_mqtt_broker"`   // MQTT broker URL (e.g., ssl://mqtt.cloudloop.com:8883)
	CloudloopMQTTCACert         string `yaml:"cloudloop_mqtt_ca_cert"`  // Path to CA cert PEM
	CloudloopMQTTCert           string `yaml:"cloudloop_mqtt_cert"`     // Path to client cert PEM
	CloudloopMQTTKey            string `yaml:"cloudloop_mqtt_key"`      // Path to client key PEM
	CloudloopAccountID          string `yaml:"cloudloop_account_id"`    // Cloudloop account ID for MQTT topic

	// Bridge lifecycle
	BridgeOfflineTimeout   int    `yaml:"bridge_offline_timeout"`     // seconds without health before marking offline (default 300)
	BridgeCACertExportPath string `yaml:"bridge_ca_cert_export_path"` // path to export bridge CA cert for NATS mTLS (empty=disabled)
	// BridgeCASecretName is a pre-created Kubernetes Secret (in POD_NAMESPACE)
	// the Hub keeps equal to its bridge CA certificate for NATS/stunnel mTLS;
	// empty disables the writer. Key defaults to ca.crt.
	BridgeCASecretName string `yaml:"bridge_ca_secret_name"`
	BridgeCASecretKey  string `yaml:"bridge_ca_secret_key"`
	// NATSAuthSecretName is the pre-created Secret that receives the rendered NATS
	// users/permissions file (internal/bridge/natsauth.go); empty disables it.
	NATSAuthSecretName string `yaml:"nats_auth_secret_name"`

	// WireGuard (wg-easy)
	WGEnabled  bool   `yaml:"wg_enabled"`
	WGURL      string `yaml:"wg_url"`      // wg-easy base URL (e.g., http://wg-easy:51821)
	WGPassword string `yaml:"wg_password"` // wg-easy web UI password

	// Observability
	PprofEnabled       bool   `yaml:"pprof_enabled"`         // Enable /debug/pprof/* endpoints (default false)
	DBSlowQueryMS      int    `yaml:"db_slow_query_ms"`      // Slow query threshold in milliseconds (default 100)
	DBRetryMaxAttempts int    `yaml:"db_retry_max_attempts"` // Bound on transient DB error retries per operation (default 8)
	AuditRetentionDays int    `yaml:"audit_retention_days"`  // Days to keep audit log entries (default 90, 0=disabled)
	AuditArchivePath   string `yaml:"audit_archive_path"`    // Path to archive purged audit entries as JSONL (empty=no archive)
	// S3-compatible archive for purged audit entries (MR 22); when the endpoint,
	// bucket and keys are set it replaces AuditArchivePath.
	AuditArchiveS3Endpoint  string `yaml:"audit_archive_s3_endpoint"`
	AuditArchiveS3Bucket    string `yaml:"audit_archive_s3_bucket"`
	AuditArchiveS3Prefix    string `yaml:"audit_archive_s3_prefix"`
	AuditArchiveS3Region    string `yaml:"audit_archive_s3_region"`
	AuditArchiveS3AccessKey string `yaml:"-"`
	AuditArchiveS3SecretKey string `yaml:"-"`
	// Self-hosted vector basemap (MESHSAT-967): one PMTiles archive in an
	// S3-compatible bucket, streamed by the Hub at /basemap/basemap.pmtiles.
	// Endpoint, bucket, region and keys fall back to the audit archive values,
	// which point at the same store; an empty key disables the route.
	BasemapS3Endpoint string `yaml:"basemap_s3_endpoint"`
	BasemapS3Bucket   string `yaml:"basemap_s3_bucket"`
	BasemapS3Region   string `yaml:"basemap_s3_region"`
	BasemapS3Key      string `yaml:"basemap_s3_key"`
	// A second, deeper archive covering the area the fleet operates in. The
	// world archive is necessarily shallow: street geometry needs zoom 13 and
	// street names zoom 15, which is only affordable regionally (MESHSAT-967).
	// Empty means the map is world-only.
	BasemapS3LocalKey string `yaml:"basemap_s3_local_key"`
	// Key prefix of the glyph ranges and sprite sheets the map style needs
	// (default "basemap/assets"), served at /basemap/assets/.
	BasemapS3AssetPrefix string `yaml:"basemap_s3_asset_prefix"`
	BasemapS3AccessKey   string `yaml:"-"`
	BasemapS3SecretKey   string `yaml:"-"`
	BasemapCacheMaxAge   string `yaml:"basemap_cache_max_age"`  // Cache-Control max-age of the archive (default "24h")
	HealthProbeTimeout   string `yaml:"health_probe_timeout"`   // Health probe timeout duration (default "3s")
	ShutdownDrainSeconds int    `yaml:"shutdown_drain_seconds"` // Seconds /readyz reports draining before the listener closes (default 0)
	OTelEndpoint         string `yaml:"otel_endpoint"`          // OTLP HTTP endpoint (empty=disabled)
	OTelServiceName      string `yaml:"otel_service_name"`      // OTel service name (default "meshsat-hub")
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Port:                  6070,
		MQTTBrokerURL:         "tcp://mqtt:1883",
		Mode:                  "standalone",
		MQTTClientID:          "meshsat-hub",
		CloudloopAPIURL:       "https://api.cloudloop.com",
		GlobalstarAPIURL:      "https://api.globalstar.com/v1",
		LogLevel:              "info",
		LogFormat:             "json",
		RateLimitBurst:        10,
		RateLimitRefillPerMin: 1.0,
		RateLimitDailyCap:     100,
		ReticulumIdentityFile: "data/reticulum_identity.json",
		ReticulumAppName:      "meshsat.hub",
		ReticulumTCPEnabled:   true,
		ReticulumTCPAddr:      ":4242",
		BridgeOfflineTimeout:  300, // 5 minutes
		DBSlowQueryMS:         100,
		OIDCScopes:            "openid profile email",
		OIDCGroupsClaim:       "groups",
		OIDCAdminGroup:        "meshsat-platform-admin",
		SQLitePath:            "/data/hub.db",
		DBRetryMaxAttempts:    8,
		AuditRetentionDays:    90,
		HealthProbeTimeout:    "3s",
		ShutdownDrainSeconds:  0,
		OTelServiceName:       "meshsat-hub",
		AuthRateLimitPerMin:   30,
		StripeTimeout:         20 * time.Second,
		MailFrom:              "billing@meshsat.net",
		MailFromName:          "MeshSat Hub",
		MailTimeout:           15 * time.Second,
		PublicURL:             "https://hub.meshsat.net",
		// Receipts: Dutch 21% VAT, inclusive, EUR, NL. The rate lives here
		// rather than in code so re-pricing or a rate change is a ConfigMap
		// edit; the URL and token stay unset until an operator supplies them.
		InvoiceNinjaTaxName:   "BTW 21",
		InvoiceNinjaTaxRate:   21,
		InvoiceNinjaCurrency:  "EUR",
		InvoiceNinjaCountryID: "528",
		InvoiceNinjaTimeout:   30 * time.Second,
	}
}

// Load reads configuration from a YAML file (if it exists) and applies
// environment variable overrides. Environment variables use the HUB_ prefix.
func Load() (Config, error) {
	cfg := Defaults()

	// Read YAML file if specified or default exists.
	path := envOr("HUB_CONFIG_FILE", "config.yaml")
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// Environment variable overrides.
	if v := os.Getenv("HUB_MODE"); v != "" {
		cfg.Mode = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_DATABASE_URL"); v != "" {
		cfg.DatabaseURL = v
	}
	if v := os.Getenv("HUB_DB_DRIVER"); v != "" {
		cfg.DBDriver = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_SQLITE_PATH"); v != "" {
		cfg.SQLitePath = v
	}
	if v := os.Getenv("HUB_REDIS_URL"); v != "" {
		cfg.RedisURL = v
	}
	if v := os.Getenv("HUB_NATS_URL"); v != "" {
		cfg.NATSUrl = v
	}
	if v := os.Getenv("HUB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("HUB_MQTT_BROKER_URL"); v != "" {
		cfg.MQTTBrokerURL = v
	}
	if v := os.Getenv("HUB_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTTClientID = v
	}
	if v := os.Getenv("HUB_ROCKBLOCK_SECRET"); v != "" {
		cfg.RockBLOCKSecret = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_API_KEY"); v != "" {
		cfg.CloudloopAPIKey = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_API_URL"); v != "" {
		cfg.CloudloopAPIURL = v
	}
	if v := os.Getenv("HUB_GLOBALSTAR_API_KEY"); v != "" {
		cfg.GlobalstarAPIKey = v
	}
	if v := os.Getenv("HUB_GLOBALSTAR_API_URL"); v != "" {
		cfg.GlobalstarAPIURL = v
	}
	if v := os.Getenv("HUB_GLOBALSTAR_WEBHOOK_SECRET"); v != "" {
		cfg.GlobalstarWebhookSecret = v
	}
	if v := os.Getenv("HUB_LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_LOG_FORMAT"); v != "" {
		cfg.LogFormat = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_AUTH_TOKEN"); v != "" {
		cfg.AuthToken = v
	}
	if v := os.Getenv("HUB_AUTH_MODE"); v != "" {
		cfg.AuthMode = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_JWT_SIGNING_KEY"); v != "" {
		cfg.JWTSigningKey = v
	}
	if v := os.Getenv("HUB_OIDC_ISSUER_URL"); v != "" {
		cfg.OIDCIssuerURL = v
	}
	if v := os.Getenv("HUB_OIDC_CLIENT_ID"); v != "" {
		cfg.OIDCClientID = v
	}
	if v := os.Getenv("HUB_OIDC_CLIENT_SECRET"); v != "" {
		cfg.OIDCClientSecret = v
	}
	if v := os.Getenv("HUB_OIDC_REDIRECT_URI"); v != "" {
		cfg.OIDCRedirectURI = v
	}
	if v := os.Getenv("HUB_OIDC_SCOPES"); v != "" {
		cfg.OIDCScopes = v
	}
	if v := os.Getenv("HUB_OIDC_GROUPS_CLAIM"); v != "" {
		cfg.OIDCGroupsClaim = v
	}
	if v := os.Getenv("HUB_OIDC_ADMIN_GROUP"); v != "" {
		cfg.OIDCAdminGroup = v
	}
	if v := os.Getenv("HUB_OIDC_BOOTSTRAP_OWNER_EMAIL"); v != "" {
		cfg.OIDCBootstrapOwnerEmail = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("HUB_OIDC_SIGNUP_URL"); v != "" {
		cfg.OIDCSignupURL = v
	}
	if v := os.Getenv("HUB_AUTHENTIK_URL"); v != "" {
		cfg.AuthentikURL = v
	}
	if v := os.Getenv("HUB_AUTHENTIK_TOKEN"); v != "" {
		cfg.AuthentikToken = v
	}
	if v := os.Getenv("HUB_COMMUNITY_URL"); v != "" {
		cfg.CommunityURL = v
	}
	if v := os.Getenv("HUB_LOCAL_LOGIN_ENABLED"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		cfg.LocalLoginEnabled = &b
	}
	if v := os.Getenv("HUB_METRICS_TOKEN"); v != "" {
		cfg.MetricsToken = v
	}
	if v := os.Getenv("HUB_OIDC_AUDIENCE"); v != "" {
		cfg.OIDCAudience = v
	}

	// OIDC cert pinning overrides
	if v := os.Getenv("HUB_OIDC_CERT_PIN"); v != "" {
		cfg.OIDCCertPin = v
	}
	if v := os.Getenv("HUB_OIDC_CERT_PIN_BACKUP"); v != "" {
		cfg.OIDCCertPinBackup = v
	}

	// TLS pin overrides
	if v := os.Getenv("HUB_TLS_PIN_PRIMARY"); v != "" {
		cfg.TLSPinPrimary = v
	}
	if v := os.Getenv("HUB_TLS_PIN_BACKUP"); v != "" {
		cfg.TLSPinBackup = v
	}

	for _, plan := range []string{"free", "crew", "fleet", "custom"} {
		v := os.Getenv("HUB_PLAN_" + strings.ToUpper(plan) + "_DEVICES")
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < -1 {
			continue
		}
		if cfg.PlanDeviceLimits == nil {
			cfg.PlanDeviceLimits = map[string]int{}
		}
		cfg.PlanDeviceLimits[plan] = n
	}
	// The TAK account ceiling, read the same way and held to the same rules: an
	// unparseable value or anything below -1 is ignored rather than guessed at,
	// so a typo in the ConfigMap cannot hand out an accidental allowance.
	for _, plan := range []string{"free", "crew", "fleet", "custom"} {
		v := os.Getenv("HUB_PLAN_" + strings.ToUpper(plan) + "_TAK_USERS")
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < -1 {
			continue
		}
		if cfg.PlanTAKUserLimits == nil {
			cfg.PlanTAKUserLimits = map[string]int{}
		}
		cfg.PlanTAKUserLimits[plan] = n
	}
	// The hosted TAK front (MESHSAT-1037).
	if v := os.Getenv("HUB_TAK_FRONT_ENABLED"); v != "" {
		cfg.TAKFrontEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("HUB_TAK_FRONT_ADDR"); v != "" {
		cfg.TAKFrontAddr = v
	}
	if v := os.Getenv("HUB_TAK_FRONT_CERT_FILE"); v != "" {
		cfg.TAKFrontCertFile = v
	}
	if v := os.Getenv("HUB_TAK_FRONT_KEY_FILE"); v != "" {
		cfg.TAKFrontKeyFile = v
	}
	if v := os.Getenv("HUB_TAK_NAMESPACE"); v != "" {
		cfg.TAKNamespace = v
	}
	if v := os.Getenv("HUB_OIDC_RECOVERY_URL"); v != "" {
		cfg.OIDCRecoveryURL = v
	}
	if v := os.Getenv("HUB_OIDC_PASSWORD_CHANGE_URL"); v != "" {
		cfg.OIDCPasswordChangeURL = v
	}
	if v := os.Getenv("HUB_OIDC_MFA_SETUP_URL"); v != "" {
		cfg.OIDCMFASetupURL = v
	}
	if v := os.Getenv("HUB_AUTH_RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.AuthRateLimitPerMin = n
		}
	}
	if v := os.Getenv("HUB_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = v
	}
	// Stripe (MESHSAT-1023). Every one of these is refused when it is the
	// literal string "<no value>": the ExternalSecret renders that for a key
	// missing from the backing store, and it is NOT empty, so every `!= ""`
	// guard in this Hub would treat it as configured and come up believing it
	// could take money.
	if v := stripeSecret("HUB_STRIPE_SECRET_KEY"); v != "" {
		cfg.StripeSecretKey = v
	}
	if v := stripeSecret("HUB_STRIPE_WEBHOOK_SECRET"); v != "" {
		cfg.StripeWebhookSecret = v
	}
	if v := stripeSecret("HUB_STRIPE_PATH_SECRET"); v != "" {
		cfg.StripePathSecret = v
	}
	if v := stripeSecret("HUB_STRIPE_PUBLISHABLE_KEY"); v != "" {
		cfg.StripePublishableKey = v
	}
	if v := os.Getenv("HUB_STRIPE_DONATION_PRICE"); v != "" {
		cfg.StripeDonationPrice = strings.TrimSpace(v)
	}
	if v := os.Getenv("HUB_STRIPE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.StripeTimeout = d
		}
	}
	// "price_abc=crew,price_def=fleet"
	if v := os.Getenv("HUB_STRIPE_PRICES"); v != "" {
		m := map[string]string{}
		for _, pair := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			if k = strings.TrimSpace(k); k != "" {
				m[k] = strings.TrimSpace(val)
			}
		}
		if len(m) > 0 {
			cfg.StripePrices = m
		}
	}
	if v := os.Getenv("HUB_INVOICENINJA_URL"); v != "" {
		cfg.InvoiceNinjaURL = v
	}
	if v := os.Getenv("HUB_INVOICENINJA_TOKEN"); v != "" {
		cfg.InvoiceNinjaToken = v
	}
	if v := os.Getenv("HUB_INVOICENINJA_TAX_NAME"); v != "" {
		cfg.InvoiceNinjaTaxName = v
	}
	if v := os.Getenv("HUB_INVOICENINJA_TAX_RATE"); v != "" {
		// Strictly greater than zero. A 0 was accepted before and is never what
		// anybody meant: it issues every receipt with a 0% BTW line, which is a
		// wrong document rather than a missing one, and nothing downstream can
		// tell the difference (MESHSAT-1016).
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.InvoiceNinjaTaxRate = f
		} else {
			slog.Error("config: HUB_INVOICENINJA_TAX_RATE is not a positive number; keeping the default",
				"got", v, "using", cfg.InvoiceNinjaTaxRate)
		}
	}
	if v := os.Getenv("HUB_INVOICENINJA_CURRENCY"); v != "" {
		cfg.InvoiceNinjaCurrency = v
	}
	if v := os.Getenv("HUB_INVOICENINJA_COUNTRY_ID"); v != "" {
		cfg.InvoiceNinjaCountryID = v
	}
	if v := os.Getenv("HUB_INVOICENINJA_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.InvoiceNinjaTimeout = d
		}
	}
	if v := os.Getenv("HUB_SMTP_RELAY"); v != "" {
		cfg.SMTPRelay = v
	}
	if v := os.Getenv("HUB_MAIL_FROM"); v != "" {
		cfg.MailFrom = v
	}
	if v := os.Getenv("HUB_MAIL_FROM_NAME"); v != "" {
		cfg.MailFromName = v
	}
	if v := os.Getenv("HUB_MAIL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.MailTimeout = d
		}
	}
	if v := os.Getenv("HUB_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("HUB_UPGRADE_URL"); v != "" {
		cfg.UpgradeURL = v
	}
	// APRS-IS overrides
	if v := os.Getenv("HUB_APRSIS_ENABLED"); v != "" {
		cfg.APRSISEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_APRSIS_SERVER"); v != "" {
		cfg.APRSISServer = v
	}
	if v := os.Getenv("HUB_APRSIS_CALLSIGN"); v != "" {
		cfg.APRSISCallsign = v
	}
	if v := os.Getenv("HUB_APRSIS_PASSCODE"); v != "" {
		cfg.APRSISPasscode = v
	}

	// Rate limit overrides
	if v := os.Getenv("HUB_RATELIMIT_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimitBurst = n
		}
	}
	if v := os.Getenv("HUB_RATELIMIT_REFILL_PER_MIN"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.RateLimitRefillPerMin = f
		}
	}
	if v := os.Getenv("HUB_RATELIMIT_DAILY_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimitDailyCap = n
		}
	}
	if v := os.Getenv("HUB_RATELIMIT_MONTHLY_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimitMonthlyCap = n
		}
	}

	// Tenant isolation overrides. On the cluster (HUB_MODE=kubernetes) the
	// SaaS runs with isolation on unless explicitly switched off.
	if v := os.Getenv("HUB_TENANT_ENFORCE"); v != "" {
		cfg.TenantEnforce = strings.EqualFold(v, "true") || v == "1"
	} else if cfg.Mode == "kubernetes" {
		cfg.TenantEnforce = true
	}

	// Apprise overrides
	if v := os.Getenv("HUB_APPRISE_ENABLED"); v != "" {
		cfg.AppriseEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_APPRISE_URL"); v != "" {
		cfg.AppriseURL = v
	}

	// ntfy overrides
	if v := os.Getenv("HUB_NTFY_ENABLED"); v != "" {
		cfg.NtfyEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_NTFY_URL"); v != "" {
		cfg.NtfyURL = v
	}
	if v := os.Getenv("HUB_NTFY_TOKEN"); v != "" {
		cfg.NtfyToken = v
	}

	// Cluster peers override
	if v := os.Getenv("HUB_CLUSTER_PEERS"); v != "" {
		cfg.ClusterPeers = v
	}

	// hawkBit OTA overrides
	if v := os.Getenv("HUB_HAWKBIT_ENABLED"); v != "" {
		cfg.HawkBitEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_HAWKBIT_URL"); v != "" {
		cfg.HawkBitURL = v
	}
	if v := os.Getenv("HUB_HAWKBIT_USERNAME"); v != "" {
		cfg.HawkBitUsername = v
	}
	if v := os.Getenv("HUB_HAWKBIT_PASSWORD"); v != "" {
		cfg.HawkBitPassword = v
	}

	// Rock7 overrides
	if v := os.Getenv("HUB_ROCK7_USERNAME"); v != "" {
		cfg.Rock7Username = v
	}
	if v := os.Getenv("HUB_ROCK7_PASSWORD"); v != "" {
		cfg.Rock7Password = v
	}

	// SOS overrides
	if v := os.Getenv("HUB_SOS_CHAIN_ID"); v != "" {
		cfg.SOSChainID = v
	}

	// Email overrides
	if v := os.Getenv("HUB_EMAIL_ENABLED"); v != "" {
		cfg.EmailEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_EMAIL_SMTP_HOST"); v != "" {
		cfg.EmailSMTPHost = v
	}
	if v := os.Getenv("HUB_EMAIL_FROM"); v != "" {
		cfg.EmailFrom = v
	}
	if v := os.Getenv("HUB_EMAIL_USERNAME"); v != "" {
		cfg.EmailUsername = v
	}
	if v := os.Getenv("HUB_EMAIL_PASSWORD"); v != "" {
		cfg.EmailPassword = v
	}
	if v := os.Getenv("HUB_EMAIL_PGP_KEY"); v != "" {
		cfg.EmailPGPKey = v
	}
	if v := os.Getenv("HUB_EMAIL_WEBHOOK_SECRET"); v != "" {
		cfg.EmailWebhookSecret = v
	}
	// OOB defaults then overrides.
	cfg.OOBEncrypt = true
	cfg.OOBMaxPerHour = 20
	cfg.OOBSMSTimeout = 60 * time.Second
	cfg.OOBSatTimeout = 10 * time.Minute
	if v := os.Getenv("HUB_OOB_ENCRYPT"); v != "" {
		cfg.OOBEncrypt = v == "true" || v == "1"
	}
	if v := os.Getenv("HUB_OOB_MAX_PER_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.OOBMaxPerHour = n
		}
	}
	if v := os.Getenv("HUB_OOB_SMS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.OOBSMSTimeout = d
		}
	}
	if v := os.Getenv("HUB_OOB_SAT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.OOBSatTimeout = d
		}
	}

	// SMS overrides
	if v := os.Getenv("HUB_SMS_ENABLED"); v != "" {
		cfg.SMSEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_SMS_ACCOUNT_SID"); v != "" {
		cfg.SMSAccountSID = v
	}
	if v := os.Getenv("HUB_SMS_AUTH_TOKEN"); v != "" {
		cfg.SMSAuthToken = v
	}
	if v := os.Getenv("HUB_SMS_API_KEY_SID"); v != "" {
		cfg.SMSAPIKeySID = v
	}
	if v := os.Getenv("HUB_SMS_FROM_NUMBER"); v != "" {
		cfg.SMSFromNumber = v
	}
	if v := os.Getenv("HUB_SMS_WEBHOOK_SECRET"); v != "" {
		cfg.SMSWebhookSecret = v
	}

	// Reticulum overrides
	if v := os.Getenv("HUB_RETICULUM_IDENTITY_FILE"); v != "" {
		cfg.ReticulumIdentityFile = v
	}
	if v := os.Getenv("HUB_RETICULUM_APP_NAME"); v != "" {
		cfg.ReticulumAppName = v
	}
	if v := os.Getenv("HUB_RETICULUM_TCP_ENABLED"); v != "" {
		cfg.ReticulumTCPEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_RETICULUM_TCP_ADDR"); v != "" {
		cfg.ReticulumTCPAddr = v
	}
	// Legacy: HUB_RETICULUM_TCP_PORT sets just the port (e.g. "4242" → ":4242").
	if v := os.Getenv("HUB_RETICULUM_TCP_PORT"); v != "" {
		cfg.ReticulumTCPEnabled = true
		cfg.ReticulumTCPAddr = ":" + v
	}

	// Cloudloop MO webhook/MQTT overrides
	if v := os.Getenv("HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS"); v != "" {
		cfg.CloudloopWebhookAllowedIPs = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_WEBHOOK_EXPECTED_IPS"); v != "" {
		cfg.CloudloopWebhookExpectedIPs = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_WEBHOOK_TOKEN"); v != "" {
		cfg.CloudloopWebhookToken = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_MQTT_BROKER"); v != "" {
		cfg.CloudloopMQTTBroker = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_MQTT_CA_CERT"); v != "" {
		cfg.CloudloopMQTTCACert = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_MQTT_CERT"); v != "" {
		cfg.CloudloopMQTTCert = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_MQTT_KEY"); v != "" {
		cfg.CloudloopMQTTKey = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_ACCOUNT_ID"); v != "" {
		cfg.CloudloopAccountID = v
	}

	// Bridge lifecycle overrides
	if v := os.Getenv("HUB_BRIDGE_OFFLINE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BridgeOfflineTimeout = n
		}
	}

	// Bridge CA cert export path override
	if v := os.Getenv("HUB_BRIDGE_CA_SECRET_NAME"); v != "" {
		cfg.BridgeCASecretName = v
	}
	if v := os.Getenv("HUB_NATS_AUTH_SECRET_NAME"); v != "" {
		cfg.NATSAuthSecretName = v
	}
	if v := os.Getenv("HUB_BRIDGE_CA_SECRET_KEY"); v != "" {
		cfg.BridgeCASecretKey = v
	}
	if v := os.Getenv("HUB_BRIDGE_CA_CERT_EXPORT_PATH"); v != "" {
		cfg.BridgeCACertExportPath = v
	}

	// Observability overrides
	if v := os.Getenv("HUB_PPROF_ENABLED"); v != "" {
		cfg.PprofEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_DB_SLOW_QUERY_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.DBSlowQueryMS = n
		}
	}
	if v := os.Getenv("HUB_DB_RETRY_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			cfg.DBRetryMaxAttempts = n
		}
	}
	if v := os.Getenv("HUB_SHUTDOWN_DRAIN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.ShutdownDrainSeconds = n
		}
	}
	if v := os.Getenv("HUB_AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AuditRetentionDays = n
		}
	}
	if v := os.Getenv("HUB_AUDIT_ARCHIVE_PATH"); v != "" {
		cfg.AuditArchivePath = v
	}
	for env, dst := range map[string]*string{
		"HUB_AUDIT_ARCHIVE_S3_ENDPOINT":   &cfg.AuditArchiveS3Endpoint,
		"HUB_AUDIT_ARCHIVE_S3_BUCKET":     &cfg.AuditArchiveS3Bucket,
		"HUB_AUDIT_ARCHIVE_S3_PREFIX":     &cfg.AuditArchiveS3Prefix,
		"HUB_AUDIT_ARCHIVE_S3_REGION":     &cfg.AuditArchiveS3Region,
		"HUB_AUDIT_ARCHIVE_S3_ACCESS_KEY": &cfg.AuditArchiveS3AccessKey,
		"HUB_AUDIT_ARCHIVE_S3_SECRET_KEY": &cfg.AuditArchiveS3SecretKey,
		"HUB_BASEMAP_S3_ENDPOINT":         &cfg.BasemapS3Endpoint,
		"HUB_BASEMAP_S3_BUCKET":           &cfg.BasemapS3Bucket,
		"HUB_BASEMAP_S3_REGION":           &cfg.BasemapS3Region,
		"HUB_BASEMAP_S3_KEY":              &cfg.BasemapS3Key,
		"HUB_BASEMAP_S3_LOCAL_KEY":        &cfg.BasemapS3LocalKey,
		"HUB_BASEMAP_S3_ASSET_PREFIX":     &cfg.BasemapS3AssetPrefix,
		"HUB_BASEMAP_S3_ACCESS_KEY":       &cfg.BasemapS3AccessKey,
		"HUB_BASEMAP_S3_SECRET_KEY":       &cfg.BasemapS3SecretKey,
		"HUB_BASEMAP_CACHE_MAX_AGE":       &cfg.BasemapCacheMaxAge,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("HUB_HEALTH_PROBE_TIMEOUT"); v != "" {
		cfg.HealthProbeTimeout = v
	}
	if v := os.Getenv("HUB_OTEL_ENDPOINT"); v != "" {
		cfg.OTelEndpoint = v
	}
	if v := os.Getenv("HUB_OTEL_SERVICE_NAME"); v != "" {
		cfg.OTelServiceName = v
	}

	// WireGuard overrides
	if v := os.Getenv("HUB_WG_ENABLED"); v != "" {
		cfg.WGEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_WG_URL"); v != "" {
		cfg.WGURL = v
	}
	if v := os.Getenv("HUB_WG_PASSWORD"); v != "" {
		cfg.WGPassword = v
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ResolvedDBDriver returns "sqlite" or "postgres": HUB_DB_DRIVER when set,
// else sniffed from the DSN (postgres:// -> postgres), else the mode default
// (postgres in cluster/kubernetes mode, sqlite otherwise). MariaDB/Galera
// support was removed with the notrf01 cutover (MESHSAT-864 MR 23).
func (c Config) ResolvedDBDriver() string {
	switch c.DBDriver {
	case "sqlite", "postgres":
		return c.DBDriver
	}
	dsn := strings.ToLower(c.DatabaseURL)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "postgres"
	}
	if c.Mode == "cluster" || c.Mode == "kubernetes" {
		return "postgres"
	}
	return "sqlite"
}

// stripeSecret reads a secret that arrives through the ExternalSecret, and
// refuses the one value that looks configured and is not.
//
// k8s External Secrets renders a template reference to a key missing from the
// backing store as the literal four-word string "<no value>". It is not empty,
// so `!= ""` accepts it, and the Hub would boot announcing that billing was on
// while holding a credential that cannot work. That has already cost this
// codebase one silent misconfiguration (MESHSAT-998); refusing it here costs
// one comparison.
func stripeSecret(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return ""
	}
	if v == "<no value>" {
		slog.Error("config: this key is missing from the secret store, so its "+
			"ExternalSecret rendered the literal \"<no value>\"; treating it as unset "+
			"rather than booting with a credential that cannot work", "var", name)
		return ""
	}
	return v
}
