// Package store defines the persistence interface for MeshSat Hub.
// Two implementations: sqlite (standalone mode) and mariadb (cluster/k8s mode).
// Selected at startup based on HUB_MODE config.
package store

import (
	"context"
	"time"
)

// DefaultTenantID is used when no tenant context is available (single-tenant mode).
const DefaultTenantID = "default"

// Store is the persistence interface for all Hub durable state.
// Both SQLite and MariaDB implement this interface.
// All tenant-scoped methods accept a tenantID parameter for strict data isolation.
type Store interface {
	// Lifecycle
	Migrate(ctx context.Context) error
	Close() error
	Ping(ctx context.Context) error

	// Devices
	CreateDevice(ctx context.Context, tenantID string, d *Device) error
	GetDevice(ctx context.Context, tenantID string, imei string) (*Device, error)
	ListDevices(ctx context.Context, tenantID string) ([]Device, error)
	UpdateDevice(ctx context.Context, tenantID string, d *Device) error
	DeleteDevice(ctx context.Context, tenantID string, imei string) error
	TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error

	// Messages (MO + MT)
	InsertMessage(ctx context.Context, tenantID string, m *Message) error
	ListMessages(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]Message, error)
	GetMessage(ctx context.Context, tenantID string, id string) (*Message, error)

	// Webhooks (outbound config)
	SaveWebhook(ctx context.Context, tenantID string, w *WebhookConfig) error
	ListWebhooks(ctx context.Context, tenantID string) ([]WebhookConfig, error)
	DeleteWebhook(ctx context.Context, tenantID string, id string) error

	// Webhook delivery logs
	InsertDeliveryLog(ctx context.Context, tenantID string, l *DeliveryLog) error
	ListDeliveryLogs(ctx context.Context, tenantID string, limit int) ([]DeliveryLog, error)

	// Positions
	InsertPosition(ctx context.Context, tenantID string, p *Position) error
	LatestPosition(ctx context.Context, tenantID string, deviceIMEI string) (*Position, error)
	ListPositions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]Position, error)
	ListPositionsRange(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit, offset int) ([]Position, int, error)

	// Audit log
	InsertAuditEntry(ctx context.Context, tenantID string, a *AuditEntry) error
	ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error)
	GetLatestAuditEntry(ctx context.Context, tenantID string) (*AuditEntry, error)

	// Device config versioning
	CreateDeviceConfig(ctx context.Context, tenantID string, c *DeviceConfig) error
	GetDeviceConfigLatest(ctx context.Context, tenantID string, deviceIMEI string) (*DeviceConfig, error)
	GetDeviceConfigVersion(ctx context.Context, tenantID string, deviceIMEI string, version int) (*DeviceConfig, error)
	ListDeviceConfigVersions(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]DeviceConfig, error)

	// Escalation chains
	CreateEscalationChain(ctx context.Context, tenantID string, c *EscalationChain) error
	GetEscalationChain(ctx context.Context, tenantID string, id string) (*EscalationChain, error)
	ListEscalationChains(ctx context.Context, tenantID string) ([]EscalationChain, error)
	DeleteEscalationChain(ctx context.Context, tenantID string, id string) error

	// Alerts
	CreateAlert(ctx context.Context, tenantID string, a *Alert) error
	GetAlert(ctx context.Context, tenantID string, id string) (*Alert, error)
	ListAlerts(ctx context.Context, tenantID string, activeOnly bool, limit int) ([]Alert, error)
	UpdateAlert(ctx context.Context, tenantID string, a *Alert) error

	// Notification preferences (per-device Apprise URLs)
	SaveNotificationPref(ctx context.Context, tenantID string, p *NotificationPref) error
	GetNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) (*NotificationPref, error)
	ListNotificationPrefs(ctx context.Context, tenantID string) ([]NotificationPref, error)
	DeleteNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) error

	// Users (local accounts)
	CreateUser(ctx context.Context, tenantID string, u *LocalUser) error
	GetUserByID(ctx context.Context, tenantID string, id string) (*LocalUser, error)
	GetUserByEmail(ctx context.Context, tenantID string, email string) (*LocalUser, error)
	ListUsers(ctx context.Context, tenantID string) ([]LocalUser, error)
	UpdateUser(ctx context.Context, tenantID string, u *LocalUser) error
	DeleteUser(ctx context.Context, tenantID string, id string) error
	IncrementFailedLogins(ctx context.Context, tenantID string, id string) (int, error)
	ResetFailedLogins(ctx context.Context, tenantID string, id string) error

	// Refresh tokens
	StoreRefreshToken(ctx context.Context, tenantID string, t *RefreshToken) error
	GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, tokenHash string) error
	DeleteRefreshTokensByUser(ctx context.Context, tenantID string, userID string) error

	// API keys
	CreateAPIKey(ctx context.Context, tenantID string, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, string, error) // returns key + tenantID
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, tenantID string, id string) error
	TouchAPIKeyLastUsed(ctx context.Context, id string) error

	// Device encryption keys
	CreateDeviceKey(ctx context.Context, tenantID string, k *DeviceKey) error
	ListDeviceKeys(ctx context.Context, tenantID string, deviceIMEI string) ([]DeviceKey, error)
	GetDeviceKeyLatest(ctx context.Context, tenantID string, deviceIMEI string) (*DeviceKey, error)
	DeleteDeviceKey(ctx context.Context, tenantID string, id string) error
}

// Device represents a registered field device.
type Device struct {
	IMEI      string    `json:"imei"`
	Label     string    `json:"label"`
	Type      string    `json:"type"` // "rockblock", "astrocast", etc.
	Notes     string    `json:"notes,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message represents an MO or MT satellite message.
type Message struct {
	ID         string    `json:"id"`
	DeviceIMEI string    `json:"device_imei"`
	Direction  string    `json:"direction"` // "mo" or "mt"
	Channel    string    `json:"channel"`   // "iridium", "astrocast"
	MOMSN      int       `json:"momsn,omitempty"`
	Text       string    `json:"text,omitempty"`
	RawHex     string    `json:"raw_hex,omitempty"`
	Compressed bool      `json:"compressed"`
	Status     string    `json:"status"` // "received", "queued", "sent", "delivered", "failed"
	Error      string    `json:"error,omitempty"`
	Lat        float64   `json:"lat,omitempty"`
	Lon        float64   `json:"lon,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// WebhookConfig defines an outbound webhook target.
type WebhookConfig struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Secret     string    `json:"secret,omitempty"`
	Events     []string  `json:"events"`
	MaxRetries int       `json:"max_retries"`
	TimeoutSec int       `json:"timeout_sec"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// DeliveryLog records a webhook delivery attempt.
type DeliveryLog struct {
	ID         string    `json:"id"`
	WebhookID  string    `json:"webhook_id"`
	Event      string    `json:"event"`
	DeviceIMEI string    `json:"device_imei"`
	StatusCode int       `json:"status_code"`
	Error      string    `json:"error,omitempty"`
	Attempt    int       `json:"attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

// Position is a device GPS/CEP position record.
type Position struct {
	ID         string    `json:"id"`
	DeviceIMEI string    `json:"device_imei"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	Alt        float64   `json:"alt,omitempty"`
	Speed      float64   `json:"speed,omitempty"`   // m/s
	Heading    float64   `json:"heading,omitempty"` // degrees 0-360
	Sats       int       `json:"sats,omitempty"`    // satellites in view
	Source     string    `json:"source"`            // "gps", "iridium_cep", "astrocast"
	CEP        float64   `json:"cep,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AuditEntry records a security-relevant action with hash-chain tamper evidence.
type AuditEntry struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Detail    string    `json:"detail,omitempty"`
	IP        string    `json:"ip,omitempty"`
	PrevHash  string    `json:"prev_hash"` // SHA-256 hash of the previous entry
	Hash      string    `json:"hash"`      // SHA-256 of (action|actor|detail|ip|prev_hash)
	CreatedAt time.Time `json:"created_at"`
}

// DeviceConfig represents a versioned configuration snapshot for a field device.
type DeviceConfig struct {
	ID         string    `json:"id"`
	DeviceIMEI string    `json:"device_imei"`
	Version    int       `json:"version"`
	Config     string    `json:"config"`  // JSON-encoded configuration
	Author     string    `json:"author"`  // who made this change
	Comment    string    `json:"comment"` // change description
	CreatedAt  time.Time `json:"created_at"`
}

// EscalationTier defines a notification tier within an escalation chain.
type EscalationTier struct {
	Name       string   `json:"name"`        // e.g. "sms_oncall", "email_team", "page_manager"
	Targets    []string `json:"targets"`     // notification targets (URLs, emails, phone numbers)
	WaitSec    int      `json:"wait_sec"`    // seconds to wait before escalating to next tier
	MaxRetries int      `json:"max_retries"` // max delivery retries within this tier
}

// EscalationChain defines an ordered set of notification tiers for alert handling.
type EscalationChain struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Tiers     []EscalationTier `json:"tiers"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// Alert states.
const (
	AlertStateTriggered    = "triggered"
	AlertStateEscalating   = "escalating"
	AlertStateAcknowledged = "acknowledged"
	AlertStateExhausted    = "exhausted"
)

// Alert represents an active or resolved escalation alert.
type Alert struct {
	ID          string    `json:"id"`
	ChainID     string    `json:"chain_id"`
	DeviceIMEI  string    `json:"device_imei"`
	Type        string    `json:"type"`         // "sos", "deadman", "geofence", "custom"
	Detail      string    `json:"detail"`       // human-readable description
	State       string    `json:"state"`        // triggered, escalating, acknowledged, exhausted
	CurrentTier int       `json:"current_tier"` // 0-indexed tier in the chain
	Retries     int       `json:"retries"`      // retries within current tier
	AckedBy     string    `json:"acked_by,omitempty"`
	AckedAt     time.Time `json:"acked_at,omitempty"`
	NextEscAt   time.Time `json:"next_esc_at"` // when to escalate to next tier
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NotificationPref stores per-device Apprise notification URLs and settings.
type NotificationPref struct {
	ID         string    `json:"id"`
	DeviceIMEI string    `json:"device_imei"` // device IMEI or "*" for tenant-wide default
	URLs       []string  `json:"urls"`        // Apprise notification URLs (e.g., "slack://token", "mailto://...")
	Events     []string  `json:"events"`      // event types to notify on: "sos", "deadman", "geofence", "mo", "mt_status"
	Enabled    bool      `json:"enabled"`     // toggle notifications without deleting config
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// LocalUser represents a locally managed user account.
type LocalUser struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`    // Argon2id hash — NEVER exposed in JSON
	Role         string    `json:"role"` // "viewer", "operator", "owner"
	Enabled      bool      `json:"enabled"`
	FailedLogins int       `json:"failed_logins,omitempty"`
	LockedUntil  time.Time `json:"locked_until,omitempty"`
	LastLoginAt  time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RefreshToken is a hashed refresh token stored in the database.
type RefreshToken struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	TokenHash string    `json:"-"` // SHA-256 hash — never expose plaintext
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// MaxFailedLogins is the threshold before account lockout.
const MaxFailedLogins = 10

// LockoutDuration is how long an account is locked after MaxFailedLogins.
const LockoutDuration = 30 * time.Minute

// DeviceKey represents a per-device encryption key for E2E encrypted satellite messaging.
// Key material (KeyHex) is stored only in "decrypt" mode; in "passthrough" mode only the hash is kept.
type DeviceKey struct {
	ID         string    `json:"id"`
	DeviceIMEI string    `json:"device_imei"`
	KeyHash    string    `json:"key_hash"`          // SHA-256 hash for identification
	KeyHex     string    `json:"key_hex,omitempty"` // hex-encoded AES-256 key (omitted in listings, passthrough)
	Mode       string    `json:"mode"`              // "decrypt" (server can read) or "passthrough" (opaque)
	CreatedAt  time.Time `json:"created_at"`
}

// APIKey represents a tenant-scoped API key for programmatic access.
type APIKey struct {
	ID         string    `json:"id"`
	KeyHash    string    `json:"-"`                     // SHA-256 hash of the full key (never exposed)
	KeyPrefix  string    `json:"key_prefix"`            // first 8 chars for display (e.g. "meshsat_ab12cd34")
	Role       string    `json:"role"`                  // "viewer", "operator", "owner"
	Label      string    `json:"label"`                 // human-readable label
	DeviceIMEI string    `json:"device_imei,omitempty"` // optional: scope to specific device
	LastUsed   time.Time `json:"last_used,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
