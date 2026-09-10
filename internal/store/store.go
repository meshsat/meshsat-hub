// Package store defines the persistence interface for MeshSat Hub.
// Two implementations: sqlite (standalone mode) and mariadb (cluster/k8s mode).
// Selected at startup based on HUB_MODE config.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrDuplicate is returned by InsertMessage when a row with the same ID
// already exists. With stable message IDs (mo-{imei}-{momsn}, sms-in-{sid},
// ...) a second replica or a webhook retry inserting the same message is a
// no-op, not a second row.
var ErrDuplicate = errors.New("store: duplicate")

// ErrNotFound is returned by the tenant lookups when no row matches.
var ErrNotFound = errors.New("store: not found")

// ErrAmbiguousTenant is returned by the tenant lookups when the same device
// IMEI or bridge ID exists in more than one tenant.
var ErrAmbiguousTenant = errors.New("store: id present in several tenants")

// ReadinessProber is implemented by stores that can say whether they accept
// writes right now (Galera wsrep_ready, Postgres not in recovery). Stores
// without it are probed with Ping.
type ReadinessProber interface {
	Ready(ctx context.Context) error
}

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
	// LookupDeviceTenant returns the tenant that owns the device with this
	// IMEI (ErrNotFound when none, ErrAmbiguousTenant when several).
	LookupDeviceTenant(ctx context.Context, imei string) (string, error)
	ListDevices(ctx context.Context, tenantID string) ([]Device, error)
	// CountBillableDevices counts the devices a tenant provisioned, excluding
	// artefact types created by integrations rather than bought by anyone
	// (see ArtefactDeviceTypes). Live count rather than a stored counter: a
	// bridge changing hands and a tenant purge both move rows without going
	// through one place, so a counter would drift.
	CountBillableDevices(ctx context.Context, tenantID string) (int, error)
	UpdateDevice(ctx context.Context, tenantID string, d *Device) error
	DeleteDevice(ctx context.Context, tenantID string, imei string) error
	TouchDeviceLastSeen(ctx context.Context, tenantID string, imei string) error

	// Messages (MO + MT)
	InsertMessage(ctx context.Context, tenantID string, m *Message) error
	ListMessages(ctx context.Context, tenantID string, deviceIMEI string, limit int) ([]Message, error)
	GetMessage(ctx context.Context, tenantID string, id string) (*Message, error)
	ListScheduledMessages(ctx context.Context, before time.Time, limit int) ([]Message, error)
	UpdateMessageStatus(ctx context.Context, tenantID string, id string, status string, errMsg string) error

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
	ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]AuditEntry, error)
	DeleteAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time) (int64, error)

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
	GetAPIKeyByID(ctx context.Context, tenantID string, id string) (*APIKey, error)
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error)
	ListExpiringAPIKeys(ctx context.Context, before time.Time, limit int) ([]APIKey, error)
	UpdateAPIKeySecret(ctx context.Context, tenantID string, id string, keyHash, keyPrefix string, expiresAt time.Time) error
	DeleteAPIKey(ctx context.Context, tenantID string, id string) error
	TouchAPIKeyLastUsed(ctx context.Context, id string) error

	// Device encryption keys
	CreateDeviceKey(ctx context.Context, tenantID string, k *DeviceKey) error
	ListDeviceKeys(ctx context.Context, tenantID string, deviceIMEI string) ([]DeviceKey, error)
	GetDeviceKeyLatest(ctx context.Context, tenantID string, deviceIMEI string) (*DeviceKey, error)
	DeleteDeviceKey(ctx context.Context, tenantID string, id string) error

	// Device WireGuard peer tracking
	SaveDeviceWireguard(ctx context.Context, tenantID string, dw *DeviceWireguard) error
	GetDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) (*DeviceWireguard, error)
	DeleteDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) error

	// Message routing rules
	CreateRoute(ctx context.Context, tenantID string, r *Route) error
	GetRoute(ctx context.Context, tenantID string, id string) (*Route, error)
	ListRoutes(ctx context.Context, tenantID string) ([]Route, error)
	UpdateRoute(ctx context.Context, tenantID string, r *Route) error
	DeleteRoute(ctx context.Context, tenantID string, id string) error

	// Bridges
	CreateOrUpdateBridge(ctx context.Context, tenantID string, b *Bridge) error
	GetBridge(ctx context.Context, tenantID string, bridgeID string) (*Bridge, error)
	// LookupBridgeTenant returns the tenant that owns the bridge (ErrNotFound /
	// ErrAmbiguousTenant as for LookupDeviceTenant).
	LookupBridgeTenant(ctx context.Context, bridgeID string) (string, error)
	ListBridges(ctx context.Context, tenantID string) ([]*Bridge, error)
	// CountBridges counts a tenant's bridges, which share the device ceiling.
	CountBridges(ctx context.Context, tenantID string) (int, error)
	UpdateBridge(ctx context.Context, tenantID string, bridgeID string, updates BridgeUpdate) error
	DeleteBridge(ctx context.Context, tenantID string, bridgeID string) error
	SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
	TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error
	// SetBridgeLastReport records the bearer and time of the latest report
	// from the bridge (MESHSAT-964).
	SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error

	// OOB management pairings (MESHSAT-964 C).
	UpsertOOBPeer(ctx context.Context, p *OOBPeer) error
	GetOOBPeer(ctx context.Context, tenantID string, bridgeID string) (*OOBPeer, error)
	// ListOOBPeersByPeerID returns every pairing with the 16-bit wire peer id,
	// across tenants (ids can collide; the caller tries the keys).
	ListOOBPeersByPeerID(ctx context.Context, peerID int) ([]OOBPeer, error)
	DeleteOOBPeer(ctx context.Context, tenantID string, bridgeID string) error
	// NextOOBCounter atomically increments and returns the Hub's transmit counter.
	NextOOBCounter(ctx context.Context, tenantID string, bridgeID string) (int64, error)
	// SetOOBReplayWindow persists the receive window after an accepted frame.
	SetOOBReplayWindow(ctx context.Context, tenantID string, bridgeID string, high int64, window int64) error
	SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
	AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error
	MarkStaleBridgesOffline(ctx context.Context, timeout time.Duration) (int64, error)

	// Bridge MQTT credentials
	SetBridgeCredentials(ctx context.Context, tenantID, bridgeID, username, passwordHash string) error
	GetBridgeCredentials(ctx context.Context, tenantID, bridgeID string) (*BridgeCredentials, error)
	SetBridgeCertificate(ctx context.Context, tenantID, bridgeID, certPEM string, expiry time.Time) error
	ListBridgesWithCredentials(ctx context.Context) ([]*Bridge, error)

	// HeMB bond groups (MESHSAT-429, MESHSAT-487)
	CreateBondGroup(ctx context.Context, tenantID, bridgeID string, g *BondGroup) error
	GetBondGroup(ctx context.Context, tenantID, bridgeID, groupID string) (*BondGroup, error)
	GetBondGroups(ctx context.Context, tenantID, bridgeID string) ([]BondGroup, error)
	UpdateBondGroup(ctx context.Context, tenantID, bridgeID string, g *BondGroup) error
	DeleteBondGroup(ctx context.Context, tenantID, bridgeID, groupID string) error

	// Cost ledger
	InsertCostEntry(ctx context.Context, tenantID string, c *CostEntry) error
	ListCostEntries(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit int) ([]CostEntry, error)
	AggregateCosts(ctx context.Context, tenantID string, from, to time.Time, groupBy string) ([]CostAggregate, error)

	// System config (key-value settings, e.g. hub identity keys)
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error

	// Device groups (fleet organization)
	CreateDeviceGroup(ctx context.Context, tenantID string, g *DeviceGroup) error
	GetDeviceGroup(ctx context.Context, tenantID string, id string) (*DeviceGroup, error)
	ListDeviceGroups(ctx context.Context, tenantID string) ([]DeviceGroup, error)
	UpdateDeviceGroup(ctx context.Context, tenantID string, g *DeviceGroup) error
	DeleteDeviceGroup(ctx context.Context, tenantID string, id string) error
	AddDeviceToGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error
	RemoveDeviceFromGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error
	ListDevicesInGroup(ctx context.Context, tenantID string, groupID string) ([]Device, error)
	ListGroupsForDevice(ctx context.Context, tenantID string, deviceIMEI string) ([]DeviceGroup, error)

	// Message templates
	CreateMessageTemplate(ctx context.Context, tenantID string, t *MessageTemplate) error
	GetMessageTemplate(ctx context.Context, tenantID string, id string) (*MessageTemplate, error)
	ListMessageTemplates(ctx context.Context, tenantID string) ([]MessageTemplate, error)
	UpdateMessageTemplate(ctx context.Context, tenantID string, t *MessageTemplate) error
	DeleteMessageTemplate(ctx context.Context, tenantID string, id string) error

	// Alert rules (configurable alerting engine, MESHSAT-313)
	CreateAlertRule(ctx context.Context, tenantID string, r *AlertRule) error
	GetAlertRule(ctx context.Context, tenantID string, id string) (*AlertRule, error)
	ListAlertRules(ctx context.Context, tenantID string) ([]AlertRule, error)
	UpdateAlertRule(ctx context.Context, tenantID string, r *AlertRule) error
	DeleteAlertRule(ctx context.Context, tenantID string, id string) error

	// Dispatch claims (single-writer correctness across replicas, MESHSAT-711).
	// ClaimOnce records key atomically; exactly one caller across all replicas
	// gets true for a given key. PurgeClaims drops claims older than before.
	ClaimOnce(ctx context.Context, key string) (bool, error)
	PurgeClaims(ctx context.Context, before time.Time) (int64, error)
	// ClaimScheduledMessage moves a due message from "scheduled" to "sending";
	// only the replica that flipped the status gets true. ExpireStaleSends
	// fails "sending" rows whose claim is older than olderThan (a replica died).
	ClaimScheduledMessage(ctx context.Context, id string) (bool, error)
	ExpireStaleSends(ctx context.Context, olderThan time.Duration) (int64, error)
	// AdvanceAlert writes a's state/tier/retries/next_esc_at only if the stored
	// next_esc_at still equals expectedNextEscAt (compare-and-set); false means
	// another replica already advanced this alert.
	AdvanceAlert(ctx context.Context, tenantID string, a *Alert, expectedNextEscAt time.Time) (bool, error)

	// Dead man's switch configs (persisted so every replica and every pod
	// restart sees the same state).
	SaveDeadmanConfig(ctx context.Context, tenantID string, c *DeadmanConfig) error
	GetDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) (*DeadmanConfig, error)
	ListDeadmanConfigs(ctx context.Context) ([]DeadmanConfig, error)
	DeleteDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) error

	// Tenants (MESHSAT-916): one tenant per approved account.
	CreateTenant(ctx context.Context, t *Tenant) error
	GetTenant(ctx context.Context, id string) (*Tenant, error)
	GetTenantBySlug(ctx context.Context, slug string) (*Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	UpdateTenant(ctx context.Context, t *Tenant) error
	// ApplyKofiDelivery claims deliveryKey and, only if the claim is new,
	// writes the plan grant onto the tenant. It reports whether it applied;
	// false means this exact delivery was already applied and the caller must
	// not grant anything for it.
	//
	// The claim and the grant are one transaction, which buys three things a
	// read-then-write guard cannot. The webhook is not leader-gated, so two
	// replicas can serve the same retry: only one insert survives the unique
	// key. Every key ever applied is remembered, not just the last one, so a
	// retry that arrives after a later payment cannot re-apply. And if the
	// grant fails the claim rolls back with it, so Ko-fi's retry still works.
	ApplyKofiDelivery(ctx context.Context, t *Tenant, deliveryKey string) (bool, error)

	// EnsureClaimCode gives a tenant a Ko-fi claim code if it has none and
	// returns the code it actually carries afterwards. The write is
	// conditional on the column still being empty, so two concurrent first
	// readers of the usage endpoint both come away with the same code.
	// A plain read-then-write let the later write win while the earlier
	// caller was shown a code that was no longer in the database -- and a
	// payment quoting that code could never be matched to anyone
	// (MESHSAT-1005).
	EnsureClaimCode(ctx context.Context, tenantID, candidate string) (string, error)
	// SoftDeleteTenant blocks a tenant and starts the grace period. Reversible
	// with UpdateTenant until PurgeTenant runs.
	SoftDeleteTenant(ctx context.Context, id string, at time.Time) error
	// ListTenantsDeletedBefore returns tenants whose grace period has expired.
	ListTenantsDeletedBefore(ctx context.Context, cutoff time.Time) ([]Tenant, error)
	// PurgeTenant destroys every row the tenant owns, across every
	// tenant-scoped table, and the tenant itself. There is no undo.
	PurgeTenant(ctx context.Context, id string) error
	// ExportTenant returns every row the tenant owns, keyed by table, for the
	// portability half of the same promise.
	ExportTenant(ctx context.Context, id string) (map[string][]map[string]any, error)

	// Tenant invites: an owner invites an email address into a tenant with a
	// role; the invite is claimed at the invitee's first login.
	CreateInvite(ctx context.Context, tenantID string, inv *TenantInvite) error
	GetPendingInviteByEmail(ctx context.Context, email string) (*TenantInvite, error)
	AcceptInvite(ctx context.Context, id string) error
	ListInvites(ctx context.Context, tenantID string) ([]TenantInvite, error)
	DeleteInvite(ctx context.Context, tenantID string, id string) error

	// OIDC identities (MESHSAT-916): maps an IdP subject to a local user.
	LinkOIDCIdentity(ctx context.Context, id *OIDCIdentity) error
	// ClaimOIDCIdentity links an identity only if that subject is not linked
	// yet, and returns the identity that holds the subject afterwards --
	// another caller's when this one lost, together with claimed=false.
	//
	// LinkOIDCIdentity upserts, which is right for a returning user and wrong
	// for a brand new one: two callbacks for one new subject both provision a
	// tenant, and the second upsert repoints the subject at its own, leaving
	// the first fully populated, owned, counted by billing and unreachable by
	// anybody. The loser needs to know it lost (MESHSAT-1006).
	ClaimOIDCIdentity(ctx context.Context, id *OIDCIdentity) (*OIDCIdentity, bool, error)
	GetOIDCIdentity(ctx context.Context, issuer, subject string) (*OIDCIdentity, error)
	// IsPlatformAdmin reports whether any linked OIDC identity of the user carries
	// the platform-admin flag (used when a session is refreshed without the IdP).
	IsPlatformAdmin(ctx context.Context, tenantID, userID string) (bool, error)

	// Credential management (MESHSAT-356)
	CreateCredential(ctx context.Context, tenantID string, c *Credential) error
	GetCredential(ctx context.Context, tenantID string, id string) (*Credential, error)
	ListCredentials(ctx context.Context, tenantID string) ([]Credential, error)
	UpdateCredential(ctx context.Context, tenantID string, c *Credential) error
	DeleteCredential(ctx context.Context, tenantID string, id string) error
	ListExpiringCredentials(ctx context.Context, before time.Time) ([]Credential, error)
	// ListHubCredentialsByProvider returns, across all tenants, the hub-scoped
	// credentials of one provider (per-tenant provider accounts, MESHSAT-977).
	ListHubCredentialsByProvider(ctx context.Context, provider string) ([]Credential, error)

	// Receipts (MESHSAT-998): the outbox that turns a Ko-fi payment into a
	// document. CreateReceipt reports false when the delivery key is already
	// present, which is what makes a replayed webhook produce no second
	// receipt; it is the idempotency token, not a cache.
	CreateReceipt(ctx context.Context, r *Receipt) (bool, error)
	GetReceiptByKey(ctx context.Context, deliveryKey string) (*Receipt, error)
	// ListDueReceipts returns pending receipts whose next attempt is due,
	// oldest first, across every tenant. Drained by the lease holder.
	ListDueReceipts(ctx context.Context, now time.Time, limit int) ([]Receipt, error)
	// SetReceiptInvoice records the invoice a billing system created for a
	// receipt that is still pending. Written the moment the invoice exists so
	// a later failure resumes onto it: the invoice has taken a number out of
	// a gapless series, and a retry that made a second one would leave the
	// first permanently unpaid in the books.
	SetReceiptInvoice(ctx context.Context, id, invoiceRef string) error
	// MarkReceiptIssued records the document the billing system produced.
	MarkReceiptIssued(ctx context.Context, id, invoiceNumber, invoiceRef string, at time.Time) error
	// MarkReceiptAttempt records a failed attempt and when to try again. The
	// row stays pending: a receipt for money that was taken is never dropped.
	MarkReceiptAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error
	// BlockReceipt parks a receipt that needs a person (an unexpected
	// currency, no address to send it to) rather than retrying forever.
	BlockReceipt(ctx context.Context, id, reason string) error
}

// OOBPeer is the Hub's out-of-band management pairing with one bridge
// (MESHSAT-964 C): the shared AES-256 key (encrypted at rest with the
// credentials master key), the Hub's role in the key relationship, the
// bridge's bearer addresses, the Hub's transmit counter and the receive
// replay window.
type OOBPeer struct {
	TenantID  string    `json:"tenant_id"`
	BridgeID  string    `json:"bridge_id"`
	PeerID    int       `json:"peer_id"`            // derived from the key, never 0
	KeyEnc    []byte    `json:"-"`                  // AES-GCM under the master key
	LocalRole int       `json:"local_role"`         // 0 issuer (Hub generated the key), 1 importer (key came from the kit's bundle)
	Phone     string    `json:"phone,omitempty"`    // the kit's SIM, E.164, for the SMS bearer
	SatIMEI   string    `json:"sat_imei,omitempty"` // the kit's Iridium modem IMEI for MT
	TxCounter int64     `json:"tx_counter"`
	RxHigh    int64     `json:"rx_high"`
	RxWindow  int64     `json:"-"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Bridge represents a registered field bridge (parent of devices).
type Bridge struct {
	BridgeID        string     `json:"bridge_id"`
	TenantID        string     `json:"tenant_id"`
	Label           string     `json:"label"`
	Hostname        string     `json:"hostname"`
	Version         string     `json:"version"`
	Mode            string     `json:"mode"`
	LocationLat     float64    `json:"location_lat"`
	LocationLon     float64    `json:"location_lon"`
	LocationAlt     float64    `json:"location_alt"`
	Capabilities    string     `json:"capabilities"` // JSON array
	ReticulumHash   string     `json:"reticulum_hash"`
	ReticulumPubkey string     `json:"reticulum_pubkey"`
	CoTType         string     `json:"cot_type"`
	CoTCallsign     string     `json:"cot_callsign"`
	Online          bool       `json:"online"`
	LastBirth       string     `json:"last_birth"`  // JSON
	LastHealth      string     `json:"last_health"` // JSON
	LastSeen        *time.Time `json:"last_seen,omitempty"`
	// Last report received over any bearer (mqtt, sms, sbd, imt, globalstar)
	// and when: uplink frames while MQTT is down keep these current (MESHSAT-964).
	LastReportBearer string     `json:"last_report_bearer,omitempty"`
	LastReportAt     *time.Time `json:"last_report_at,omitempty"`
	MQTTUsername     string     `json:"mqtt_username,omitempty"`
	MQTTPasswordHash string     `json:"-"` // bcrypt hash — NEVER exposed in JSON
	CertPEM          string     `json:"cert_pem,omitempty"`
	CertExpiry       *time.Time `json:"cert_expiry,omitempty"`
	BirthVerified    bool       `json:"birth_verified"` // true if last birth had valid ECDSA signature
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// BridgeCredentials holds MQTT authentication info for a bridge.
type BridgeCredentials struct {
	BridgeID   string
	Username   string
	Password   string // bcrypt hash (or plaintext when returned from generation)
	CertPEM    string // PEM-encoded client certificate
	CertExpiry *time.Time
	CreatedAt  time.Time
}

// BridgeUpdate contains optional fields for partial bridge updates.
type BridgeUpdate struct {
	Label       *string `json:"label,omitempty"`
	CoTCallsign *string `json:"cot_callsign,omitempty"`
}

// BondGroup defines a HeMB bonding group for multi-path delivery.
type BondGroup struct {
	ID         string  `json:"id"`
	TenantID   string  `json:"tenant_id"`
	BridgeID   string  `json:"bridge_id"`
	Label      string  `json:"label"`
	Members    string  `json:"members"` // JSON array of interface IDs
	CostBudget float64 `json:"cost_budget"`
	CreatedAt  string  `json:"created_at"`
}

// Route defines a configurable message routing rule.
type Route struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	SourceType      string `json:"source_type"`
	DestinationType string `json:"destination_type"`
	Filter          string `json:"filter,omitempty"`
	// Senders restricts the route to messages whose origin (device IMEI or
	// phone number) is in this comma-separated list; empty = any sender
	// (MESHSAT-964). "*" matches everyone.
	Senders   string    `json:"senders,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Device represents a registered field device.
type Device struct {
	IMEI      string    `json:"imei"`
	Label     string    `json:"label"`
	Type      string    `json:"type"` // "rockblock", "globalstar", etc.
	Notes     string    `json:"notes,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message represents an MO or MT satellite message.
type Message struct {
	ID          string    `json:"id"`
	DeviceIMEI  string    `json:"device_imei"`
	Direction   string    `json:"direction"` // "mo" or "mt"
	Channel     string    `json:"channel"`   // "iridium", "globalstar"
	MOMSN       int       `json:"momsn,omitempty"`
	Text        string    `json:"text,omitempty"`
	RawHex      string    `json:"raw_hex,omitempty"`
	Compressed  bool      `json:"compressed"`
	Status      string    `json:"status"` // "received", "queued", "sent", "delivered", "failed", "scheduled"
	Error       string    `json:"error,omitempty"`
	Lat         float64   `json:"lat,omitempty"`
	Lon         float64   `json:"lon,omitempty"`
	ScheduledAt time.Time `json:"scheduled_at,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
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
	Source     string    `json:"source"`            // "gps", "iridium_cep", "globalstar"
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
	TenantID    string    `json:"tenant_id"`
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

// OIDCIdentity links an IdP (issuer, subject) pair to a local user. It is
// upserted on every OIDC login so PlatformAdmin follows group membership.
type OIDCIdentity struct {
	Issuer        string    `json:"issuer"`
	Subject       string    `json:"subject"`
	UserID        string    `json:"user_id"`
	TenantID      string    `json:"tenant_id"`
	Email         string    `json:"email"`
	PlatformAdmin bool      `json:"platform_admin"`
	LastLoginAt   time.Time `json:"last_login_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// Tenant is an isolation boundary: an organisation or an individual account.
// Every tenant-scoped row carries its ID. "default" is seeded by migrations
// so pre-tenancy data keeps a home.
type Tenant struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	Plan        string    `json:"plan"`   // beta, ...
	Status      string    `json:"status"` // active, suspended, deleted
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// PlanExpiresAt is when a paid tier lapses back to free. nil means the
	// plan does not expire, which is what free, custom and beta are.
	//
	// An expiry rather than a subscription state on purpose: Ko-fi fires a
	// webhook on payment and never on cancellation, so a payment pushes this
	// date out and a lapse is simply the date passing. Lapsing refuses NEW
	// registrations and nothing else -- every device already registered keeps
	// reporting, and its SOS path is untouched.
	PlanExpiresAt *time.Time `json:"plan_expires_at,omitempty"`
	// KofiClaimCode is the short code a supporter puts in the Ko-fi message so
	// a payment can be matched to this tenant. People pay from a different
	// address than they signed up with often enough that email alone loses
	// payments. Never logged with the payment payload.
	KofiClaimCode string `json:"kofi_claim_code,omitempty"`
	// KofiPayerEmail is the address the last matched payment came from, learned
	// when a payment first matches this tenant.
	//
	// It exists because Ko-fi carries the supporter's message only on the join
	// payment; every renewal has message null. The claim code therefore matches
	// once and never again, so the payer has to be remembered or a paying
	// subscriber lapses at day 32 while their card is still being charged.
	KofiPayerEmail string `json:"kofi_payer_email,omitempty"`
	// KofiLastMessageID is the id of the last Ko-fi delivery applied to this
	// tenant. Ko-fi retries the same message_id until it gets a 200, so a
	// response lost on the way back would otherwise buy a second month for
	// free. Compared before a payment is applied.
	KofiLastMessageID string `json:"-"`
	// LapseWarnedAt is when the customer was last warned their plan is about to
	// end. It stops an hourly job from mailing hourly: a warning counts for the
	// expiry it was sent for, so a renewal that pushes the date out re-arms it.
	LapseWarnedAt *time.Time `json:"-"`
	// DeletedAt is when the owner asked to leave. The tenant is blocked from
	// that moment and its data is destroyed after PurgeGrace, so an accidental
	// or disputed deletion is recoverable until then (MESHSAT-975 follow-on).
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Tenant lifecycle states.
const (
	TenantActive    = "active"
	TenantSuspended = "suspended"
	TenantDeleted   = "deleted"
)

// ArtefactDeviceTypes are device rows an integration creates on its own,
// which nobody bought and which must not count against a tenant's quota.
//
// "tak" is the OpenTAKServer poller mirroring ATAK markers: the rows carry
// marker UIDs in the imei column, they appear and multiply with third-party
// traffic, and 28 of the 31 devices in the platform tenant are these. Billing
// somebody for a busy TAK feed would charge them for something they did not do.
//
// A deny-list rather than an allow-list on purpose: devices.type is free-form
// with no enum, so an allow-list would silently stop counting the first time
// somebody typed a new string.
var ArtefactDeviceTypes = []string{"tak"}

// PurgeGrace is how long a deleted tenant's data survives before the purge job
// destroys it. Long enough to undo a mistake, short enough to be a real
// erasure promise.
const PurgeGrace = 30 * 24 * time.Hour

// TenantInvite lets an owner bring another account into their tenant.
type TenantInvite struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Email      string    `json:"email"`
	Role       string    `json:"role"` // viewer, operator, owner
	TokenHash  string    `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
	AcceptedAt time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ReservedTenantIDs are the first path segments of non-tenant MQTT topics;
// a tenant ID must never collide with them once topics carry the tenant.
var ReservedTenantIDs = map[string]bool{"bridge": true, "hub": true, "broadcast": true, "reticulum": true, "federation": true, "routed": true}

// ErrReservedTenantID is returned by CreateTenant for a reserved word.
var ErrReservedTenantID = errors.New("store: reserved tenant id")

// DeadmanConfig is the persisted dead man's switch state for one device.
type DeadmanConfig struct {
	DeviceIMEI   string    `json:"device_imei"`
	TenantID     string    `json:"tenant_id,omitempty"`
	ChainID      string    `json:"chain_id"`
	IntervalSec  int       `json:"interval_sec"`
	GraceSec     int       `json:"grace_sec"`
	Enabled      bool      `json:"enabled"`
	SnoozedUntil time.Time `json:"snoozed_until,omitempty"`
	Alerted      bool      `json:"alerted"`
	UpdatedAt    time.Time `json:"updated_at"`
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

// DeviceWireguard tracks the WireGuard peer provisioned for a device.
type DeviceWireguard struct {
	DeviceIMEI string    `json:"device_imei"`
	PeerID     string    `json:"peer_id"`     // wg-easy peer ID
	VPNAddress string    `json:"vpn_address"` // allocated VPN IP (e.g. "10.8.0.5/32")
	PublicKey  string    `json:"public_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// CostEntry records the cost of a single satellite message send.
type CostEntry struct {
	ID            string    `json:"id"`
	DeviceIMEI    string    `json:"device_imei"`
	InterfaceType string    `json:"interface_type"` // iridium_sbd, iridium_imt, globalstar
	Direction     string    `json:"direction"`      // mo or mt
	CostUSD       float64   `json:"cost_usd"`
	MessageID     string    `json:"message_id"`
	Detail        string    `json:"detail,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// CostAggregate holds aggregated cost data grouped by device or month.
type CostAggregate struct {
	GroupKey string  `json:"group_key"` // device IMEI or month string
	TotalUSD float64 `json:"total_usd"`
	Count    int     `json:"count"`
}

// DeviceGroup represents a named group for organizing devices in a fleet.
type DeviceGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Color       string    `json:"color"`
	MemberCount int       `json:"member_count,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MessageTemplate represents a reusable message template with variable substitution.
type MessageTemplate struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	Variables []string  `json:"variables,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// APIKey represents a tenant-scoped API key for programmatic access.
type APIKey struct {
	ID           string    `json:"id"`
	KeyHash      string    `json:"-"`                     // SHA-256 hash of the full key (never exposed)
	KeyPrefix    string    `json:"key_prefix"`            // first 8 chars for display (e.g. "meshsat_ab12cd34")
	Role         string    `json:"role"`                  // "viewer", "operator", "owner"
	Label        string    `json:"label"`                 // human-readable label
	DeviceIMEI   string    `json:"device_imei,omitempty"` // optional: scope to specific device
	LastUsed     time.Time `json:"last_used,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	RotationDays int       `json:"rotation_days,omitempty"` // auto-rotation period (0=disabled)
	CreatedAt    time.Time `json:"created_at"`
}

// AlertRule defines a configurable condition that triggers an escalation chain.
type AlertRule struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id,omitempty"`
	Name            string    `json:"name"`
	ConditionType   string    `json:"condition_type"`   // device_not_seen, battery_low, geofence_breach, message_rate_drop
	ConditionParams string    `json:"condition_params"` // JSON: {"threshold_hours":6} or {"threshold_pct":20}
	ChainID         string    `json:"chain_id"`         // escalation chain to trigger
	DeviceFilter    string    `json:"device_filter"`    // "*" for all, or specific IMEI, or group ID
	Enabled         bool      `json:"enabled"`
	LastEvaluated   time.Time `json:"last_evaluated,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Credential represents a provider certificate or credential stored encrypted at rest.
type Credential struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id,omitempty"`
	Provider        string     `json:"provider"`  // cloudloop_mqtt, rockblock, globalstar, etc.
	Name            string     `json:"name"`      // human-readable label
	CredType        string     `json:"cred_type"` // mtls_bundle, api_key, webhook_secret, username_password
	EncryptedData   []byte     `json:"-"`         // AES-256-GCM encrypted JSON (never in API responses)
	CertNotAfter    *time.Time `json:"cert_not_after,omitempty"`
	CertSubject     string     `json:"cert_subject,omitempty"`
	CertIssuer      string     `json:"cert_issuer,omitempty"`
	CertFingerprint string     `json:"cert_fingerprint,omitempty"`
	TargetScope     string     `json:"target_scope"` // hub, bridge, all
	TargetBridgeID  string     `json:"target_bridge_id,omitempty"`
	Status          string     `json:"status"` // active, expiring, expired, revoked
	Version         int        `json:"version"`
	DistributedAt   *time.Time `json:"distributed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Receipt is one payment that owes the customer a document (MESHSAT-998).
//
// It is an outbox, not a copy of the invoice. The billing system holds the
// document and the number series; this row holds the fact that money arrived,
// who it was for, and whether the document has been issued yet -- so a payment
// can never be silently lost between the webhook returning 200 and the invoice
// existing.
//
// The row is written before the plan is granted, because the receipt records
// the payment rather than the grant: if the grant fails, the money still
// arrived and the customer is still owed a receipt.
type Receipt struct {
	ID string `json:"id"`
	// TenantID is who the payment was for. Present so the row is
	// tenant-scoped: an offboarding export carries it and a purge destroys it,
	// the same as every other table with this column. The invoice itself stays
	// in the billing system, where it is kept under a legal retention duty
	// rather than the tenant's instruction.
	TenantID string `json:"tenant_id"`
	// DeliveryKey is the idempotency token, unique across the table. It is the
	// payment provider's delivery id when there is one, and a derived key when
	// there is not -- an absent id must not become a blank that collides with
	// every other blank, nor a hole that lets a replay through.
	DeliveryKey string `json:"delivery_key"`
	// TransactionID is the provider's transaction reference, for a human
	// reconciling a bank line against a document.
	TransactionID string `json:"transaction_id,omitempty"`
	// Email and Name are who the document goes to and what the customer is
	// called on it.
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	// AmountCents is the gross amount in the smallest unit of Currency. Money
	// is never a float here: the payload's decimal string is parsed to an
	// integer and stays one all the way to the invoice line.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// Plan and TierName describe what was bought, for the invoice line.
	Plan     string    `json:"plan,omitempty"`
	TierName string    `json:"tier_name,omitempty"`
	PaidAt   time.Time `json:"paid_at"`

	Status        string     `json:"status"` // ReceiptPending, ReceiptIssued, ReceiptBlocked
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt time.Time  `json:"next_attempt_at,omitempty"`
	InvoiceNumber string     `json:"invoice_number,omitempty"`
	InvoiceRef    string     `json:"invoice_ref,omitempty"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Receipt statuses.
const (
	// ReceiptPending means the document has not been issued yet. The drainer
	// keeps trying; nothing expires out of this state on its own.
	ReceiptPending = "pending"
	// ReceiptIssued means the billing system produced the document and mailed
	// it. InvoiceNumber says which one.
	ReceiptIssued = "issued"
	// ReceiptBlocked means a person has to look: an amount in a currency this
	// company does not invoice in, or no address to send it to. Retrying would
	// only produce the same answer, and guessing would produce a wrong
	// document.
	ReceiptBlocked = "blocked"
)
