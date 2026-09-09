package config

import (
	"fmt"
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
	SignupWebhookURL  string `yaml:"signup_webhook_url"`
	CommunityURL      string `yaml:"community_url"`       // MeshSat community room (Matrix) shown while an account awaits approval
	LocalLoginEnabled *bool  `yaml:"local_login_enabled"` // email/password login; default true in local mode, false in oidc mode
	MetricsToken      string `yaml:"metrics_token"`       // when set, /metrics requires this bearer token

	// TAK/CoT integration
	TAKEnabled        bool   `yaml:"tak_enabled"`
	TAKHost           string `yaml:"tak_host"`
	TAKPort           int    `yaml:"tak_port"`
	TAKSSL            bool   `yaml:"tak_ssl"`
	TAKCallsignPrefix string `yaml:"tak_callsign_prefix"`
	TAKCotStaleSec    int    `yaml:"tak_cot_stale_seconds"`

	// OTS (OpenTAKServer) REST API polling — inbound CoT relay
	TAKAPIBaseURL  string `yaml:"tak_api_base_url"` // e.g. http://192.168.192.10:8880
	TAKAPIUsername string `yaml:"tak_api_username"` // OTS login username
	TAKAPIPassword string `yaml:"tak_api_password"` // OTS login password
	TAKAPIPollSec  int    `yaml:"tak_api_poll_sec"` // poll interval seconds (default 10)
	// TAKAPIInsecureTLS skips certificate verification for the Marti API
	// (OpenTAKServer's self-signed certificate). Default true; set
	// HUB_TAK_API_INSECURE_TLS=false once the server has a trusted certificate.
	TAKAPIInsecureTLS bool `yaml:"tak_api_insecure_tls"`
	// TAKAPIMaxDevices bounds how many ATAK markers the poller will
	// auto-register as devices. These rows are excluded from the subscription
	// count (store.ArtefactDeviceTypes), so nobody is billed for them; the
	// ceiling exists so a busy or misbehaving TAK server cannot fill the
	// devices table. 0 means no ceiling.
	TAKAPIMaxDevices int `yaml:"tak_api_max_devices"`

	// PlanDeviceLimits overrides a subscription tier's combined device and
	// bridge ceiling, so a tier can be re-priced without a deploy:
	// HUB_PLAN_FREE_DEVICES, HUB_PLAN_CREW_DEVICES, HUB_PLAN_FLEET_DEVICES,
	// HUB_PLAN_CUSTOM_DEVICES. -1 means no ceiling. A tier absent here keeps
	// the built-in default (internal/plans).
	PlanDeviceLimits map[string]int `yaml:"plan_device_limits"`

	// Ko-fi subscription webhook (MESHSAT-989).
	//
	// KofiWebhookSecret is the last path segment of /api/webhook/kofi/<secret>,
	// so the endpoint is not discoverable and never appears in a log (the
	// logging middleware redacts the last segment of every webhook path).
	// KofiVerificationToken is the token Ko-fi puts in the payload; it is what
	// actually authenticates the caller. Empty means the endpoint refuses
	// everything, which is the right state for a payment endpoint nobody
	// configured.
	KofiWebhookSecret     string `yaml:"kofi_webhook_secret"`
	KofiVerificationToken string `yaml:"kofi_verification_token"`
	// KofiTierMap maps a Ko-fi tier name to a plan, for when the names on the
	// Ko-fi page do not match the plan names: "Crew Membership: crew".
	KofiTierMap map[string]string `yaml:"kofi_tier_map"`

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

	// TAK Federation v2
	TAKFederationEnabled bool     `yaml:"tak_federation_enabled"`
	TAKFederationPort    int      `yaml:"tak_federation_port"`  // default 9001
	TAKFederationPeers   []string `yaml:"tak_federation_peers"` // remote TAK server host:port
	TAKFederationCert    string   `yaml:"tak_federation_cert"`
	TAKFederationKey     string   `yaml:"tak_federation_key"`
	TAKFederationCA      string   `yaml:"tak_federation_ca"`

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
	CloudloopWebhookToken      string `yaml:"cloudloop_webhook_token"`       // shared token (?token= or X-Webhook-Token); required when the allowlist is "*"
	CloudloopMQTTBroker        string `yaml:"cloudloop_mqtt_broker"`         // MQTT broker URL (e.g., ssl://mqtt.cloudloop.com:8883)
	CloudloopMQTTCACert        string `yaml:"cloudloop_mqtt_ca_cert"`        // Path to CA cert PEM
	CloudloopMQTTCert          string `yaml:"cloudloop_mqtt_cert"`           // Path to client cert PEM
	CloudloopMQTTKey           string `yaml:"cloudloop_mqtt_key"`            // Path to client key PEM
	CloudloopAccountID         string `yaml:"cloudloop_account_id"`          // Cloudloop account ID for MQTT topic

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
		TAKAPIMaxDevices:      5000,
		AuthRateLimitPerMin:   30,
		UpgradeURL:            "https://ko-fi.com/X2S326G23T",
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
	if v := os.Getenv("HUB_SIGNUP_WEBHOOK_URL"); v != "" {
		cfg.SignupWebhookURL = v
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

	// TAK/CoT overrides
	if v := os.Getenv("HUB_TAK_ENABLED"); v != "" {
		cfg.TAKEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_TAK_HOST"); v != "" {
		cfg.TAKHost = v
	}
	if v := os.Getenv("HUB_TAK_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.TAKPort = p
		}
	}
	if v := os.Getenv("HUB_TAK_SSL"); v != "" {
		cfg.TAKSSL = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_TAK_CALLSIGN_PREFIX"); v != "" {
		cfg.TAKCallsignPrefix = v
	}
	if v := os.Getenv("HUB_TAK_COT_STALE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TAKCotStaleSec = n
		}
	}

	// OTS API overrides
	if v := os.Getenv("HUB_TAK_API_INSECURE_TLS"); v != "" {
		cfg.TAKAPIInsecureTLS = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_TAK_API_BASE_URL"); v != "" {
		cfg.TAKAPIBaseURL = v
	}
	if v := os.Getenv("HUB_TAK_API_USERNAME"); v != "" {
		cfg.TAKAPIUsername = v
	}
	if v := os.Getenv("HUB_TAK_API_PASSWORD"); v != "" {
		cfg.TAKAPIPassword = v
	}
	if v := os.Getenv("HUB_TAK_API_POLL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TAKAPIPollSec = n
		}
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
	if v := os.Getenv("HUB_AUTH_RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.AuthRateLimitPerMin = n
		}
	}
	if v := os.Getenv("HUB_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = v
	}
	if v := os.Getenv("HUB_KOFI_WEBHOOK_SECRET"); v != "" {
		cfg.KofiWebhookSecret = v
	}
	if v := os.Getenv("HUB_KOFI_VERIFICATION_TOKEN"); v != "" {
		cfg.KofiVerificationToken = v
	}
	// HUB_KOFI_TIER_MAP="Crew Membership=crew,Fleet Membership=fleet"
	if v := os.Getenv("HUB_KOFI_TIER_MAP"); v != "" {
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
			cfg.KofiTierMap = m
		}
	}
	if v := os.Getenv("HUB_UPGRADE_URL"); v != "" {
		cfg.UpgradeURL = v
	}
	if v := os.Getenv("HUB_TAK_API_MAX_DEVICES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.TAKAPIMaxDevices = n
		}
	}

	// TAK Federation overrides
	if v := os.Getenv("HUB_TAK_FEDERATION_ENABLED"); v != "" {
		cfg.TAKFederationEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_TAK_FEDERATION_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.TAKFederationPort = p
		}
	}
	if v := os.Getenv("HUB_TAK_FEDERATION_PEERS"); v != "" {
		cfg.TAKFederationPeers = strings.Split(v, ",")
	}
	if v := os.Getenv("HUB_TAK_FEDERATION_CERT"); v != "" {
		cfg.TAKFederationCert = v
	}
	if v := os.Getenv("HUB_TAK_FEDERATION_KEY"); v != "" {
		cfg.TAKFederationKey = v
	}
	if v := os.Getenv("HUB_TAK_FEDERATION_CA"); v != "" {
		cfg.TAKFederationCA = v
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
