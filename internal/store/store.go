// Package store defines the persistence interface for MeshSat Hub.
// Two implementations: sqlite (standalone mode) and postgres (cluster/k8s mode).
// Selected at startup based on HUB_MODE config.
package store

import (
	"context"
	"time"
)

// DefaultTenantID is used when no tenant context is available (single-tenant mode).
const DefaultTenantID = "default"

// Store is the persistence interface for all Hub durable state.
// Both SQLite and PostgreSQL implement this interface.
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

	// Audit log
	InsertAuditEntry(ctx context.Context, tenantID string, a *AuditEntry) error
	ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error)

	// API keys
	CreateAPIKey(ctx context.Context, tenantID string, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, string, error) // returns key + tenantID
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, tenantID string, id string) error
	TouchAPIKeyLastUsed(ctx context.Context, id string) error
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
	Source     string    `json:"source"` // "gps", "iridium_cep", "astrocast"
	CEP        float64   `json:"cep,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AuditEntry records a security-relevant action.
type AuditEntry struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Detail    string    `json:"detail,omitempty"`
	IP        string    `json:"ip,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
