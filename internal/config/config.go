package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the MeshSat Hub service.
type Config struct {
	// Tri-mode: "standalone" (default), "cluster", "kubernetes"
	Mode        string `yaml:"mode"`
	DatabaseURL string `yaml:"database_url"` // MariaDB DSN (cluster/k8s only)
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
	AstrocastAPIKey         string `yaml:"astrocast_api_key"`
	AstrocastAPIURL         string `yaml:"astrocast_api_url"`
	AstrocastWebhookSecret  string `yaml:"astrocast_webhook_secret"` // HMAC-SHA256 secret for Astrocast MO webhook verification
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

	// TAK/CoT integration
	TAKEnabled        bool   `yaml:"tak_enabled"`
	TAKHost           string `yaml:"tak_host"`
	TAKPort           int    `yaml:"tak_port"`
	TAKSSL            bool   `yaml:"tak_ssl"`
	TAKCallsignPrefix string `yaml:"tak_callsign_prefix"`
	TAKCotStaleSec    int    `yaml:"tak_cot_stale_seconds"`

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

	// Cloudloop MO webhook + MQTT subscriber
	CloudloopWebhookAllowedIPs string `yaml:"cloudloop_webhook_allowed_ips"` // comma-separated IP allowlist (default: Cloudloop IPs)
	CloudloopMQTTBroker        string `yaml:"cloudloop_mqtt_broker"`         // MQTT broker URL (e.g., ssl://mqtt.cloudloop.com:8883)
	CloudloopMQTTCACert        string `yaml:"cloudloop_mqtt_ca_cert"`        // Path to CA cert PEM
	CloudloopMQTTCert          string `yaml:"cloudloop_mqtt_cert"`           // Path to client cert PEM
	CloudloopMQTTKey           string `yaml:"cloudloop_mqtt_key"`            // Path to client key PEM
	CloudloopAccountID         string `yaml:"cloudloop_account_id"`          // Cloudloop account ID for MQTT topic

	// Bridge lifecycle
	BridgeOfflineTimeout int `yaml:"bridge_offline_timeout"` // seconds without health before marking offline (default 300)

	// WireGuard (wg-easy)
	WGEnabled  bool   `yaml:"wg_enabled"`
	WGURL      string `yaml:"wg_url"`      // wg-easy base URL (e.g., http://wg-easy:51821)
	WGPassword string `yaml:"wg_password"` // wg-easy web UI password
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Port:                  6070,
		MQTTBrokerURL:         "tcp://mqtt:1883",
		Mode:                  "standalone",
		MQTTClientID:          "meshsat-hub",
		CloudloopAPIURL:       "https://api.cloudloop.com",
		AstrocastAPIURL:       "https://api.astrocast.com/v1",
		GlobalstarAPIURL:      "https://api.globalstar.com/v1",
		LogLevel:              "info",
		LogFormat:             "json",
		RateLimitBurst:        10,
		RateLimitRefillPerMin: 1.0,
		RateLimitDailyCap:     100,
		ReticulumIdentityFile: "data/reticulum_identity.json",
		ReticulumAppName:      "meshsat.hub",
		BridgeOfflineTimeout:  300, // 5 minutes
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
	if v := os.Getenv("HUB_ASTROCAST_API_KEY"); v != "" {
		cfg.AstrocastAPIKey = v
	}
	if v := os.Getenv("HUB_ASTROCAST_API_URL"); v != "" {
		cfg.AstrocastAPIURL = v
	}
	if v := os.Getenv("HUB_ASTROCAST_WEBHOOK_SECRET"); v != "" {
		cfg.AstrocastWebhookSecret = v
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

	// Tenant isolation overrides
	if v := os.Getenv("HUB_TENANT_ENFORCE"); v != "" {
		cfg.TenantEnforce = strings.EqualFold(v, "true") || v == "1"
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

	// Cloudloop MO webhook/MQTT overrides
	if v := os.Getenv("HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS"); v != "" {
		cfg.CloudloopWebhookAllowedIPs = v
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
