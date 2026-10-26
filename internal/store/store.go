// Package store defines the persistence interface for MeshSat Hub.
// Two implementations: sqlite (standalone mode) and mariadb (cluster/k8s mode).
// Selected at startup based on HUB_MODE config.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/vat"
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

// ErrOwnedElsewhere is returned when a write names a row by an id that another
// tenant holds (MESHSAT-1307). A bridge id is unique across the platform (it
// is the bridge's NATS user name), and an upsert on it once rewrote another
// tenant's bridge.
var ErrOwnedElsewhere = errors.New("store: id belongs to another tenant")

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
	// AppendAuditEntry extends a tenant's chain atomically: the latest entry is
	// read, build derives the next one from it, and the row is written, all
	// under a store-side lock held for the tenant. Two replicas appending at
	// once therefore serialise instead of both chaining onto the same
	// predecessor, which a process-local mutex could never prevent. build is
	// given nil when the chain is empty and must fill ID and CreatedAt; the
	// unique index on (tenant_id, prev_hash) refuses a fork that gets past it.
	AppendAuditEntry(ctx context.Context, tenantID string, build func(prev *AuditEntry) (*AuditEntry, error)) error
	ListAuditEntries(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error)
	GetLatestAuditEntry(ctx context.Context, tenantID string) (*AuditEntry, error)
	ListAuditEntriesBefore(ctx context.Context, tenantID string, before time.Time, limit int) ([]AuditEntry, error)
	// ListAuditEntriesByAction returns the newest entries of ONE tenant whose
	// action is in actions, newest first. It is how the platform reads its
	// own decisions back (signup approvals and rejections, MESHSAT-1366).
	ListAuditEntriesByAction(ctx context.Context, tenantID string, actions []string, limit int) ([]AuditEntry, error)
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
	//
	// CreateOrUpdateBridge saves a birth: the row, and with it in one
	// transaction the birth's declarations and modem links (Bridge.BirthStamp,
	// Egress, Links). Nothing is written when it fails. A bridge row that
	// belongs to another tenant refuses it with ErrOwnedElsewhere. A part
	// that is older than what is stored is left as stored and is not an
	// error: the newer word stands.
	CreateOrUpdateBridge(ctx context.Context, tenantID string, b *Bridge) error
	GetBridge(ctx context.Context, tenantID string, bridgeID string) (*Bridge, error)
	// LookupBridgeTenant returns the tenant that owns the bridge (ErrNotFound /
	// ErrAmbiguousTenant as for LookupDeviceTenant).
	LookupBridgeTenant(ctx context.Context, bridgeID string) (string, error)
	ListBridges(ctx context.Context, tenantID string) ([]*Bridge, error)
	// CountBridges counts a tenant's bridges, which share the device ceiling.
	CountBridges(ctx context.Context, tenantID string) (int, error)
	// CountTAKUsers counts a tenant's TAK accounts (MESHSAT-1037). A SEPARATE
	// meter from the device ceiling: a TAK user is a person with a phone and a
	// certificate, not a piece of kit, and a small fleet can have many people
	// watching the map. Live count, for the same reason as the others -- a
	// purge and a user removal both move rows without passing through one place.
	CountTAKUsers(ctx context.Context, tenantID string) (int, error)

	// Hosted TAK (MESHSAT-1037). The operator owns the real state in the
	// TakInstance custom resource; these rows are what the Hub reads to build
	// the takfront directory and to answer "is this phone still allowed"
	// without a Kubernetes call on the connection path.
	UpsertTAKInstance(ctx context.Context, inst *TAKInstance) error
	GetTAKInstance(ctx context.Context, tenantID string) (*TAKInstance, error)
	ListTAKInstances(ctx context.Context) ([]*TAKInstance, error)
	DeleteTAKInstance(ctx context.Context, tenantID string) error
	CreateTAKUser(ctx context.Context, tenantID string, u *TAKUser) error
	GetTAKUser(ctx context.Context, tenantID string, username string) (*TAKUser, error)
	ListTAKUsers(ctx context.Context, tenantID string) ([]*TAKUser, error)
	UpdateTAKUser(ctx context.Context, tenantID string, u *TAKUser) error
	DeleteTAKUser(ctx context.Context, tenantID string, username string) error
	UpdateBridge(ctx context.Context, tenantID string, bridgeID string, updates BridgeUpdate) error
	// DeleteBridge removes the bridge and its declarations and unlinks its
	// modems, now: a birth received before the delete and written later
	// cannot link them again.
	DeleteBridge(ctx context.Context, tenantID string, bridgeID string) error
	SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error
	TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error
	// SetBridgeLastReport records the bearer and time of the latest report
	// from the bridge (MESHSAT-964).
	SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error
	// RecordBridgeHealth is what a health report does to the row, in ONE
	// statement: last_health, last_seen, last_report (bearer, at), online.
	// It used to be four UPDATEs of the same row per report, from both
	// replicas, under synchronous replication; pg_stat_statements showed
	// them as the only slow statements on the platform, up to 1.4 s of
	// row-lock waiting behind each other (MESHSAT-1155).
	RecordBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string, bearer string, at time.Time) error

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
	// AssociateDeviceWithBridge links a modem to the bridge that reports it,
	// now: the link carries the time, which orders it against a birth the
	// Hub retries later (CreateOrUpdateBridge, Bridge.Links).
	AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error
	// DeviceBridgeID answers which bridge owns a modem: "" when none does or
	// the device is unknown (MESHSAT-1246, the MO receipt).
	DeviceBridgeID(ctx context.Context, tenantID string, imei string) (string, error)
	// SetDeviceEgress records, per modem, the satellite payload format its
	// bridge declared ("sealed" or "plain", HeMB-NG S3-2) and that bridge. The
	// format recorded is the bridge's stored declaration for the modem
	// (BridgeEgressEntry) when it has one, read in the same statement, so a
	// late or repeated call never records an older word than the bridge's
	// newest; format is recorded only when the bridge has none. Only a
	// registered device row takes it.
	SetDeviceEgress(ctx context.Context, tenantID, imei, bridgeID, format string) error
	// DeviceEgress is the last declaration recorded for a modem: the bridge
	// that made it and the format, both "" when there is none.
	DeviceEgress(ctx context.Context, tenantID, imei string) (bridgeID, format string, err error)
	// BridgeEgress is the format the bridge declared last for a modem, ""
	// for none.
	BridgeEgress(ctx context.Context, tenantID, bridgeID, imei string) (string, error)
	// BridgeEgressEntry is that declaration with the stamp of the birth that
	// made it; the zero EgressDecl for none.
	BridgeEgressEntry(ctx context.Context, tenantID, bridgeID, imei string) (EgressDecl, error)
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
	// ListSystemConfigOlderThan returns the keys under prefix whose row has not
	// been written since cutoff. Added for the stash reaper: until MESHSAT-1098
	// there was no way to enumerate system_config at all, so an unclaimed
	// provisioning bundle -- a plaintext MQTT password and a client private key
	// -- had no path out of the table or out of the backups.
	ListSystemConfigOlderThan(ctx context.Context, prefix string, cutoff time.Time) ([]string, error)
	// DeleteSystemConfig removes a row outright. Callers that merely CONSUME a
	// value keep blanking it (SetSystemConfig(key, "")), which is what the
	// claim paths do; this is for the sweeper, where leaving an empty row per
	// abandoned provisioning attempt would grow without bound.
	DeleteSystemConfig(ctx context.Context, key string) error
	// CompareAndSetSystemConfig writes value only while key holds old ("" is
	// an absent or blank row) and reports whether it wrote: of the replicas
	// that read the same value, exactly one swaps it. Added for the directory
	// key rotation, whose pending key must be stored by one caller and taken
	// by every other (MESHSAT-1584 r19 C7-R5).
	CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error)

	// Reticulum path table (MESHSAT-1555): the upstream-compatible node's
	// paths, persisted so the leader that takes over answers path requests
	// without waiting for every peer to announce again. Platform-wide: a
	// destination hash carries no tenant (see RNSPath).
	UpsertRNSPath(ctx context.Context, p *RNSPath) error
	ListRNSPaths(ctx context.Context) ([]RNSPath, error)
	DeleteRNSPath(ctx context.Context, destHash string) error

	// LXMF inbox and outbox (MESHSAT-1555), keyed (tenant_id, id).
	// InsertLXMFMessage returns ErrDuplicate for an id the tenant holds.
	InsertLXMFMessage(ctx context.Context, tenantID string, m *LXMFMessage) error
	GetLXMFMessage(ctx context.Context, tenantID, id string) (*LXMFMessage, error)
	ListLXMFMessages(ctx context.Context, tenantID string, q LXMFQuery) ([]LXMFMessage, error)
	// UpdateLXMFMessageState returns ErrNotFound when the tenant has no such row.
	UpdateLXMFMessageState(ctx context.Context, tenantID, id string, upd LXMFStateUpdate) error
	CountLXMFMessages(ctx context.Context, tenantID string) (int, error)
	// PruneLXMFMessages deletes the oldest rows beyond keep, never one that
	// is still queued or sending; it returns how many went.
	PruneLXMFMessages(ctx context.Context, tenantID string, keep int) (int, error)

	// Messages a tenant's LXMF propagation node holds for offline clients
	// (MESHSAT-1555 P5), keyed (tenant_id, id = transient id).
	// InsertLXMFHeld returns ErrDuplicate for a transient id the tenant holds.
	InsertLXMFHeld(ctx context.Context, tenantID string, m *LXMFHeldMessage) error
	// ListLXMFHeldFor lists what is held for one destination, smallest first,
	// without Data.
	ListLXMFHeldFor(ctx context.Context, tenantID, destination string) ([]LXMFHeldMessage, error)
	GetLXMFHeld(ctx context.Context, tenantID, id string) (*LXMFHeldMessage, error)
	// DeleteLXMFHeld removes the given ids when they are held for destination.
	DeleteLXMFHeld(ctx context.Context, tenantID, destination string, ids []string) (int, error)
	// LXMFHeldBytes is the tenant's held total.
	LXMFHeldBytes(ctx context.Context, tenantID string) (int64, error)
	// PruneLXMFHeld drops what was received before cutoff, then the oldest
	// rows until the tenant holds at most maxBytes; it returns how many went.
	PruneLXMFHeld(ctx context.Context, tenantID string, cutoff time.Time, maxBytes int64) (int, error)

	// The Hub app route hub_msg (HeMB-NG stage 2, C4, MESHSAT-1584): operations keyed
	// (tenant, sender, op_id), each its own outbox entry, and their evidence (C6).
	//
	// AdmitMsgOp stores a new operation and its sender's mid in one transaction. A known
	// key answers MsgOpExisting with the stored row, and a known mid with another content
	// digest MsgOpMidConflict; neither stores anything. Two replicas admitting the same
	// operation at once store it once.
	AdmitMsgOp(ctx context.Context, op *MsgOp) (MsgOpAdmission, error)
	// GetMsgOp returns ErrNotFound for an unknown key.
	GetMsgOp(ctx context.Context, tenantID, sender, opID string) (*MsgOp, error)
	// AddMsgOpEvent adds one event to its operation and stores the operation's new
	// projection, in one transaction: ev.Rev becomes the operation's next revision, and
	// project gets every event of the operation, the new one included. False, and nothing
	// changed, when the event id is already stored. ErrNotFound for an unknown operation.
	AddMsgOpEvent(ctx context.Context, ev *MsgOpEvent, project func([]MsgOpEvent) (state string, conflict bool)) (bool, error)
	// ListMsgOpEvents lists an operation's events by revision.
	ListMsgOpEvents(ctx context.Context, tenantID, sender, opID string) ([]MsgOpEvent, error)
	// ListDueMsgOps lists operations the outbox should publish now: not final, no answer
	// from the recipient, due, not leased, not past their deadline.
	ListDueMsgOps(ctx context.Context, now time.Time, limit int) ([]MsgOp, error)
	// ClaimMsgOp leases one operation to one drainer until the given time; false means
	// another drainer holds it at now. The caller's clock decides, as in ListDueMsgOps.
	ClaimMsgOp(ctx context.Context, tenantID, sender, opID string, now, until time.Time) (bool, error)
	// MarkMsgOpAttempt counts a publication, sets when to publish again and drops the lease.
	MarkMsgOpAttempt(ctx context.Context, tenantID, sender, opID string, next time.Time) error
	// SetMsgOpRecipientAnswer records the recipient's answer ("stored" or "refused"); the
	// outbox publishes the operation no more.
	SetMsgOpRecipientAnswer(ctx context.Context, tenantID, sender, opID, answer string) error
	// ListExpiredMsgOps lists operations that are not final and whose deadline is past.
	ListExpiredMsgOps(ctx context.Context, now time.Time, limit int) ([]MsgOp, error)
	// ListUnpublishedMsgOpEvents lists events the senders' msg/status has not taken yet.
	ListUnpublishedMsgOpEvents(ctx context.Context, limit int) ([]MsgOpEvent, error)
	MarkMsgOpEventsPublished(ctx context.Context, eventIDs []string, at time.Time) error
	// ReplayMsgOpEvents marks every event of an operation unpublished again, so a sender
	// that repeated its submission gets all of them (C4); it returns how many.
	ReplayMsgOpEvents(ctx context.Context, tenantID, sender, opID string) (int, error)
	// PruneMsgOps deletes operations whose deadline was before cutoff, with their events
	// and the mids no operation uses any more; it returns how many operations went.
	PruneMsgOps(ctx context.Context, cutoff time.Time) (int, error)

	// SOS runs (HeMB-NG stage 2, C10, MESHSAT-1584), keyed (tenant, origin, sos_id).
	//
	// ApplySOSRunEvent applies one start or cancel in one transaction, so two replicas
	// applying the same event agree. A start makes an active run, or adds its bearer to
	// a known one whatever its state. A cancel makes a cancelled run when none exists
	// (terminal before its start), ends an active one, and adds its bearer otherwise.
	// Nothing ever makes a cancelled run active again.
	ApplySOSRunEvent(ctx context.Context, ev *SOSRunEvent) (SOSRunApplied, error)
	// GetSOSRun returns ErrNotFound for an unknown key.
	GetSOSRun(ctx context.Context, tenantID, origin, sosID string) (*SOSRun, error)
	// FindSOSRuns lists a tenant's runs with this sos_id, whatever their origin.
	FindSOSRuns(ctx context.Context, tenantID, sosID string) ([]SOSRun, error)
	// ListSOSRuns lists a tenant's runs, newest first.
	ListSOSRuns(ctx context.Context, tenantID string, limit int) ([]SOSRun, error)
	// SetSOSRunAlert records the run's escalation alert.
	SetSOSRunAlert(ctx context.Context, tenantID, origin, sosID, alertID string) error

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
	// ClearSOSRunAlertPending clears a run's alert_pending once its alert work is done, but
	// only while the run's rev is still rev: an event applied meanwhile keeps it owed. False
	// when the rev moved on (or the run is gone).
	ClearSOSRunAlertPending(ctx context.Context, tenantID, origin, sosID string, rev int) (bool, error)
	// ListSOSRunsAlertPending lists, across tenants, the runs whose alert work is still owed,
	// oldest change first (the reconciler, C10).
	ListSOSRunsAlertPending(ctx context.Context, limit int) ([]SOSRun, error)
	// AdvanceAlert writes a's state/tier/retries/next_esc_at only if the stored
	// next_esc_at still equals expectedNextEscAt (compare-and-set) and the alert is
	// still open (triggered or escalating); false means another replica already
	// advanced it, or it was acknowledged or cancelled meanwhile and stays so.
	AdvanceAlert(ctx context.Context, tenantID string, a *Alert, expectedNextEscAt time.Time) (bool, error)

	// Dead man's switch configs (persisted so every replica and every pod
	// restart sees the same state).
	// Geofences (MESHSAT-1119). Tenant-scoped throughout: a fence polygon is
	// the area a customer operates in.
	// Email PGP contacts (MESHSAT-1123). Tenant-scoped: a correspondent's
	// public key belongs to the tenant that vouched for it, and an address is
	// only unique within a tenant.
	// TTC booth flow (MESHSAT-1175). See the BoothRelay type for why the
	// correlation lives here rather than in memory.
	GetBoothSession(ctx context.Context, tenantID, sender, channel string) (*BoothSession, error)
	SaveBoothSession(ctx context.Context, s *BoothSession) error
	CreateBoothRelay(ctx context.Context, r *BoothRelay) error
	GetBoothRelayByRef(ctx context.Context, tenantID, ref string) (*BoothRelay, error)
	OpenBoothRelaysFor(ctx context.Context, tenantID, bridgeID, meshDest string) ([]BoothRelay, error)
	CloseBoothRelay(ctx context.Context, tenantID, ref string) error
	CountBoothRelaysBySender(ctx context.Context, tenantID, sender string, since time.Time) (int, error)
	CountBoothRelays(ctx context.Context, tenantID string, since time.Time) (int, error)
	// ExpiredOpenBoothRelays are conversations whose reply never came. The booth
	// tells the visitor rather than leaving them watching a silent phone.
	ExpiredOpenBoothRelays(ctx context.Context, tenantID string, now time.Time) ([]BoothRelay, error)
	// The send ledger: every message the booth puts on a bearer, so spend has a
	// ceiling. Counts rows, so the budget cannot drift from what was sent.
	RecordBoothSend(ctx context.Context, tenantID, id, recipient, channel, kind string) error
	CountBoothSendsTo(ctx context.Context, tenantID, recipient string, since time.Time) (int, error)
	CountBoothSends(ctx context.Context, tenantID string, since time.Time) (int, error)
	// The "@" lane (MESHSAT-1685): a device's conversations with people, and
	// the ledger of what the lane sent. See PersonConversation.
	SavePersonConversationOut(ctx context.Context, c *PersonConversation) (*PersonConversation, error)
	GetPersonConversationByRef(ctx context.Context, tenantID, ref string) (*PersonConversation, error)
	PersonConversationsWith(ctx context.Context, tenantID, peerKind, peer string, since time.Time) ([]PersonConversation, error)
	MarkPersonConversationIn(ctx context.Context, tenantID, id string, at time.Time, messageID string) error
	MarkPersonConversationOut(ctx context.Context, tenantID, id string, at time.Time, messageID string) error
	RecordPersonChatSend(ctx context.Context, s *PersonChatSend) error
	// TakePersonChatSlot counts one use of a ceiling (key names the window,
	// "dev:<imei>:2026102412") and answers whether it was still under max.
	TakePersonChatSlot(ctx context.Context, tenantID, key string, max int) (bool, error)
	// TAK direct messages (MESHSAT-1706): the crews the tenant's clients put on
	// the TAK map and which client owns each, and what the tenant's operators
	// and the crews wrote to each other. See TAKContact and TAKChatMessage.
	UpsertTAKContact(ctx context.Context, c *TAKContact) error
	GetTAKContact(ctx context.Context, tenantID, uid string) (*TAKContact, error)
	ListTAKContacts(ctx context.Context, tenantID string, since time.Time) ([]TAKContact, error)
	SaveTAKChatMessage(ctx context.Context, m *TAKChatMessage) (bool, error)
	SetTAKChatMessageStatus(ctx context.Context, tenantID, id, status string) error
	ListTAKChatMessages(ctx context.Context, tenantID, crewUID string, limit int) ([]TAKChatMessage, error)
	// Mesh presence (MESHSAT-1181): which nodes have been heard behind which
	// bridge. See the MeshNode type for what this can and cannot prove.
	RecordMeshNode(ctx context.Context, tenantID, bridgeID, nodeID string, heardAt time.Time) error
	MeshNodesSeenSince(ctx context.Context, tenantID, bridgeID string, since time.Time) ([]MeshNode, error)

	SaveEmailContact(ctx context.Context, tenantID, email, armoredKey string) error
	ListEmailContacts(ctx context.Context, tenantID string) ([]EmailContact, error)
	DeleteEmailContact(ctx context.Context, tenantID, email string) error

	SaveGeofence(ctx context.Context, tenantID string, f *Geofence) error
	ListGeofences(ctx context.Context, tenantID string) ([]Geofence, error)
	DeleteGeofence(ctx context.Context, tenantID string, id string) error

	SaveDeadmanConfig(ctx context.Context, tenantID string, c *DeadmanConfig) error
	GetDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) (*DeadmanConfig, error)
	ListDeadmanConfigs(ctx context.Context) ([]DeadmanConfig, error)
	DeleteDeadmanConfig(ctx context.Context, tenantID string, deviceIMEI string) error

	// Tenants (MESHSAT-916): one tenant per approved account.
	CreateTenant(ctx context.Context, t *Tenant) error
	GetTenant(ctx context.Context, id string) (*Tenant, error)
	GetTenantBySlug(ctx context.Context, slug string) (*Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	// ListTenantSummaries is ListTenants with what a platform operator's
	// directory needs beside each row: the owner's address and how many users,
	// devices and bridges the tenant has. One grouped query, never one query
	// per tenant (MESHSAT-1366).
	ListTenantSummaries(ctx context.Context) ([]TenantSummary, error)
	// TenantIDsByUserEmail returns the tenants that have a user with this
	// email, compared case-insensitively, sorted (MESHSAT-1527). Cross-tenant
	// BY DESIGN, like ListTenantSummaries: it backs the platform directory's
	// email filter (GET /api/admin/tenants?email=), behind
	// RequirePlatformRead, so a platform reader can find one customer's tenant
	// without listing every tenant's users. Its subject is the tenant, which
	// is how the scoping ratchet classifies it; it returns ids only, never a
	// user row.
	TenantIDsByUserEmail(ctx context.Context, email string) ([]string, error)
	UpdateTenant(ctx context.Context, t *Tenant) error

	// TenantByStripeCustomer resolves the payment events that carry a customer
	// id and no metadata of their own (MESHSAT-1023).
	TenantByStripeCustomer(ctx context.Context, customerID string) (*Tenant, error)
	// ApplyStripeEvent records an event id and reports whether this call was
	// the one that recorded it. A compare-and-set for the same reason
	// the old provider's delivery guard was: the provider redelivers until it
	// gets a 2xx,
	// and a read-then-write lets two replicas both find a retry missing.
	ApplyStripeEvent(ctx context.Context, eventID, tenantID string) (bool, error)
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

	// Support access (MESHSAT-1366): a tenant's owner grants the platform a
	// window, with a PIN only they know, in which an operator may open their
	// workspace. See SupportGrant.
	CreateSupportGrant(ctx context.Context, tenantID string, g *SupportGrant) error
	// GetActiveSupportGrant returns the tenant's grant that is neither revoked
	// nor expired, or ErrNotFound.
	GetActiveSupportGrant(ctx context.Context, tenantID string) (*SupportGrant, error)
	// MarkSupportGrantUsed records the first opening: who, and when.
	MarkSupportGrantUsed(ctx context.Context, tenantID, id, byEmail string, at time.Time) error
	// BumpSupportGrantFailures counts a wrong PIN and returns the new count.
	BumpSupportGrantFailures(ctx context.Context, tenantID, id string) (int, error)
	// RevokeSupportGrant ends a grant early. Idempotent.
	RevokeSupportGrant(ctx context.Context, tenantID, id string, at time.Time) error

	// Federation (MESHSAT-1549): peers this tenant trusts by certificate and
	// the grants that let a peer's modem reach one of this tenant's devices.
	// CreateFederationPeer returns ErrDuplicate when the tenant already holds
	// a peer with that fingerprint.
	CreateFederationPeer(ctx context.Context, tenantID string, p *FederationPeer) error
	ListFederationPeers(ctx context.Context, tenantID string) ([]FederationPeer, error)
	// GetFederationPeer and GetFederationPeerByFingerprint return ErrNotFound
	// when the tenant holds no such peer.
	GetFederationPeer(ctx context.Context, tenantID, id string) (*FederationPeer, error)
	GetFederationPeerByFingerprint(ctx context.Context, tenantID, fingerprint string) (*FederationPeer, error)
	// DeleteFederationPeer removes the peer and every grant naming it.
	DeleteFederationPeer(ctx context.Context, tenantID, id string) error
	// CreateFederationGrant returns ErrDuplicate for an identical grant.
	CreateFederationGrant(ctx context.Context, tenantID string, g *FederationGrant) error
	ListFederationGrants(ctx context.Context, tenantID string) ([]FederationGrant, error)
	DeleteFederationGrant(ctx context.Context, tenantID, id string) error
	// HasFederationGrant reports whether the tenant lets fromIMEI on the peer
	// reach its device toIMEI.
	HasFederationGrant(ctx context.Context, tenantID, peerID, fromIMEI, toIMEI string) (bool, error)

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

	// Receipts (MESHSAT-998): the outbox that turns a payment into a
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
	// SetReceiptPaymentRef records the provider payment a receipt was settled
	// by, so a later refund can find the document to reverse. Matching on the
	// delivery key rather than the id because the caller is a webhook that
	// knows the invoice, not our row id.
	SetReceiptPaymentRef(ctx context.Context, deliveryKey, paymentRef string) error
	// GetReceiptByPaymentRef finds the receipt a refunded payment belongs to.
	// Returns ErrNotFound when nothing matches: a refund for money this Hub
	// never documented must be reported, never guessed at.
	GetReceiptByPaymentRef(ctx context.Context, paymentRef string) (*Receipt, error)
	// MarkReceiptIssued records the document the billing system produced.
	MarkReceiptIssued(ctx context.Context, id, invoiceNumber, invoiceRef string, at time.Time) error
	// MarkReceiptAttempt records a failed attempt and when to try again. The
	// row stays pending: a receipt for money that was taken is never dropped.
	MarkReceiptAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error
	// BlockReceipt parks a receipt that needs a person (an unexpected
	// currency, no address to send it to) rather than retrying forever.
	BlockReceipt(ctx context.Context, id, reason string) error
	// ClaimReceipt leases one receipt row to one drainer until the given time
	// and reports whether this caller won it. False means somebody else holds
	// it; do not touch the billing system for that row.
	//
	// A receipt draws an invoice number out of a gapless legal series, so two
	// drainers on one row create two documents and leave the first permanently
	// unpaid in the books. That was previously prevented by timing alone -- one
	// leader, a stop wait longer than the billing call, a grace period longer
	// than the stop wait. This makes it the database's decision instead, so
	// correctness stops depending on those margins holding. A drainer that dies
	// mid-issue simply lets its lease expire (MESHSAT-998).
	ClaimReceipt(ctx context.Context, id string, until time.Time) (bool, error)
	// ReleaseReceipt drops a lease early, so a receipt that failed and is due
	// again in a minute is not held for the rest of its lease.
	ReleaseReceipt(ctx context.Context, id string) error
	// CrossBorderSalesSince totals the GROSS of issued receipts to EU consumers
	// in other member states since a given instant, in minor units, grouped by
	// country.
	//
	// This is the figure the whole flat-Dutch-21% position rests on: it is only
	// correct while cross-border B2C supplies stay under the Article 59c
	// EUR 10 000 a year. Domestic Dutch sales are excluded by the caller, not
	// here -- this returns what it is asked for and internal/vat decides what
	// counts (MESHSAT-1016).
	CrossBorderSalesSince(ctx context.Context, since time.Time) (map[string]int64, error)
	// ListReceiptsByStatus returns receipts in one state, newest first. The
	// blocked ones are the reason this exists: they were terminal and
	// invisible, so money taken for a document nobody could issue simply
	// stopped being mentioned anywhere (MESHSAT-1007).
	ListReceiptsByStatus(ctx context.Context, status string, limit int) ([]Receipt, error)
	// RequeueReceipt puts a blocked receipt back in the drainer's queue after
	// a person has fixed whatever parked it. Blocked was a one-way door: no
	// API, no UI and no CLI could reopen it, so the only way to issue the
	// document was to edit the row by hand.
	RequeueReceipt(ctx context.Context, id string, at time.Time) error

	// Refunds (MESHSAT-1019): the outbox that turns money given back into a
	// credit note. It is the mirror of the receipts outbox and exists for the
	// same reason -- the billing system is a separate machine, a credit note
	// draws a number out of its own gapless series, and a customer who has had
	// their money back is owed the document that reverses the VAT.
	//
	// CreateRefund reports false when the receipt already has one: a payment is
	// refunded once, and the UNIQUE constraint on receipt_id is the check.
	CreateRefund(ctx context.Context, r *Refund) (bool, error)
	GetRefund(ctx context.Context, id string) (*Refund, error)
	GetRefundByReceipt(ctx context.Context, receiptID string) (*Refund, error)
	ListDueRefunds(ctx context.Context, now time.Time, limit int) ([]Refund, error)
	ListRefundsByStatus(ctx context.Context, status string, limit int) ([]Refund, error)
	// SetRefundCredit records the credit note the billing system created, the
	// instant it exists. A sent credit note has taken a number out of a gapless
	// series, so a retry that created a second one would leave the books with a
	// credit nobody ever received.
	SetRefundCredit(ctx context.Context, id, creditRef string) error
	MarkRefundIssued(ctx context.Context, id, creditNumber, creditRef string, at time.Time) error
	MarkRefundAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error
	BlockRefund(ctx context.Context, id, reason string) error
	RequeueRefund(ctx context.Context, id string, at time.Time) error
	ClaimRefund(ctx context.Context, id string, until time.Time) (bool, error)
	ReleaseRefund(ctx context.Context, id string) error
	// DeleteRefund removes a refund that has not produced a document yet, so an
	// operator who recorded the wrong figure can record the right one. It
	// refuses once the credit note exists, because that is a document with a
	// number and deleting the row would not unmake it.
	DeleteRefund(ctx context.Context, id string) error
	// GetReceipt reads one receipt by id. The refund path starts from a
	// receipt, so it has to be able to find one.
	GetReceipt(ctx context.Context, id string) (*Receipt, error)
	// RefundsByCountrySince totals issued refunds by country, so the VAT
	// threshold meter can subtract money that was given back. A sale that was
	// refunded is not a supply and must not count toward the threshold.
	RefundsByCountrySince(ctx context.Context, since time.Time) (map[string]int64, error)
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
// TAKInstance is the Hub's record of a tenant's own hosted OpenTAKServer
// (MESHSAT-1037).
//
// The authoritative state lives in the TakInstance custom resource the
// tak-operator reconciles; this row is a cache the Hub can read on every
// connection without asking the Kubernetes API. CACertPEM is the tenant's trust
// anchor, which is what lets takfront identify a phone's tenant from its
// certificate issuer.
type TAKInstance struct {
	TenantID string `json:"tenant_id"`
	// Label is the opaque ten-character instance name. Never the tenant's slug:
	// it appears in the CA subject, which travels to every phone, so it must
	// leak nothing about who the customer is.
	Label string `json:"label"`
	// State is what the customer asked for: Running, Suspended or Hibernated.
	State string `json:"state"`
	// Phase is what the operator reports: Provisioning, Ready, Failed and so on.
	Phase string `json:"phase"`
	// Host is host:port of the instance's CoT listener inside the cluster.
	Host      string     `json:"host"`
	CACertPEM string     `json:"ca_cert_pem"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// TAKUser is one TAK account in a tenant's instance (MESHSAT-1037).
//
// Username is what OpenTAKServer matches a client certificate's common name
// against: EudHandlerSSL reads the CN and looks up exactly this name, so a
// certificate with no matching row is refused at the CoT socket. It has no
// hyphens because OpenTAKServer's own validator rejects them with HTTP 400.
//
// EnrollTokenHash is a SHA-256 of a single-use enrollment token, never the token
// itself: the token is shown once as a QR or emailed, and storing it would put a
// live credential in every backup. It is listed in RedactedInExport too.
type TAKUser struct {
	TenantID string `json:"tenant_id"`
	Username string `json:"username"`
	Callsign string `json:"callsign"`
	// Active false keeps the row and its history while refusing the connection,
	// which is what a suspended teammate should look like.
	Active       bool       `json:"active"`
	CertSerial   string     `json:"cert_serial"`
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
	// RevokedSerial is refused by the Authorizer immediately. There is no CRL:
	// the front asks on every connection, so nothing has to be distributed.
	RevokedSerial   string     `json:"revoked_serial"`
	EnrollTokenHash string     `json:"-"`
	EnrollExpiresAt *time.Time `json:"enroll_expires_at,omitempty"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

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
	// What a birth writes besides the row, on CreateOrUpdateBridge only
	// (HeMB-NG S3-2). The store writes the row, the declarations and the
	// links in one transaction, and every part is ordered, so a birth the Hub
	// writes again later (a retry) never undoes a newer one:
	//   - BirthStamp is the birth's own timestamp (Unix ns). The row is
	//     written only when it is not older than the stored row's.
	//   - Egress is what the birth declares per modem IMEI, each with that
	//     stamp; a declaration replaces a stored one only when it is not
	//     older, so a birth that leaves a modem out keeps its declaration.
	//   - Links are the modems the birth names, each with the time (Unix ns)
	//     the Hub received the birth; a link replaces a modem's link only when
	//     that was made earlier (AssociateDeviceWithBridge links now).
	BirthStamp int64                 `json:"-"`
	Egress     map[string]EgressDecl `json:"-"`
	Links      map[string]int64      `json:"-"`
}

// EgressDecl is a declared satellite payload format ("sealed" or "plain")
// and the stamp of the birth that declared it (Bridge.BirthStamp).
type EgressDecl struct {
	Format string
	Stamp  int64
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
	// TenantID is set on rows read by platform-wide jobs (ListScheduledMessages),
	// which have no tenant of their own and must hand the row's tenant back to
	// the store when they update it (MESHSAT-1149). Tenant-scoped reads leave
	// it empty: the caller already knows.
	TenantID string `json:"tenant_id,omitempty"`
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
	ID       string `json:"id"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Detail   string `json:"detail,omitempty"`
	IP       string `json:"ip,omitempty"`
	PrevHash string `json:"prev_hash"` // SHA-256 hash of the previous entry
	Hash     string `json:"hash"`      // digest of the entry; which fields it binds is HashVersion's business
	// HashVersion names the digest Hash was computed with. 1 is the original
	// formula over action|actor|detail|ip|prev_hash, which lets a row be moved
	// between tenants or back-dated without breaking the chain; 2 also binds
	// tenant_id, id and created_at. Rows written before migration 30 carry 1
	// and stay valid; every row written since is 2, and a verifier never
	// accepts a 1 that follows a 2 (that would be a downgrade, not history).
	HashVersion int       `json:"hash_version"`
	CreatedAt   time.Time `json:"created_at"`
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
	// AlertStateCancelled: the SOS behind the alert was cancelled (C10); escalation stops
	// and nothing reopens it. AckedBy and AckedAt say who and when.
	AlertStateCancelled = "cancelled"
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
	// BridgeOfflineTimeout is how many seconds a bridge of this tenant may go
	// quiet before the reaper marks it offline. 0 means use the platform
	// default (HUB_BRIDGE_OFFLINE_TIMEOUT), which is what every tenant gets
	// until its owner chooses otherwise (MESHSAT-1117).
	BridgeOfflineTimeout int `json:"bridge_offline_timeout,omitempty"`
	// AuditRetentionDays is how long this tenant's audit entries are kept.
	// 0 means use the platform default (HUB_AUDIT_RETENTION_DAYS). The audit
	// log is the customer's own record of who did what in their account, so
	// how long they keep it is their compliance question (MESHSAT-1117).
	AuditRetentionDays int `json:"audit_retention_days,omitempty"`
	// RatelimitDailyCap and RatelimitMonthlyCap are a PLATFORM ADMIN's override
	// of this tenant's per-device send budget. 0 means none, and the plan's
	// value applies. Not owner-editable: a send budget is a commercial lever.
	RatelimitDailyCap   int `json:"ratelimit_daily_cap,omitempty"`
	RatelimitMonthlyCap int `json:"ratelimit_monthly_cap,omitempty"`
	// OOB command policy (MESHSAT-1121). 0 means use the platform default.
	// Owner-editable within platform bounds: the frames go to this tenant's own
	// field kit over a bearer this tenant is billed for, so the rate and the
	// reply timeouts are its operational choices. There is no OOBEncrypt: an OOB
	// frame is ALWAYS sealed.
	OOBMaxPerHour    int `json:"oob_max_per_hour,omitempty"`
	OOBSMSTimeoutSec int `json:"oob_sms_timeout_sec,omitempty"`
	OOBSatTimeoutSec int `json:"oob_sat_timeout_sec,omitempty"`
	// PersonChatMode is who this tenant's devices may write to through the
	// "@" lane (MESHSAT-1685): "off", "contacts" (only addresses in the
	// tenant's directory) or "open" (any number or address). Empty means the
	// platform default (HUB_PERSONCHAT_DEFAULT_MODE). The sends go out on the
	// tenant's own Twilio and mail accounts and the replies cost its own
	// satellite credit, so the choice is the tenant's.
	PersonChatMode string `json:"person_chat_mode,omitempty"`
	// PlanExpiresAt is when a paid tier lapses back to free. nil means the
	// plan does not expire, which is what free, custom and beta are.
	//
	// An expiry rather than a subscription state on purpose: Ko-fi fires a
	// webhook on payment and never on cancellation, so a payment pushes this
	// date out and a lapse is simply the date passing. Lapsing refuses NEW
	// registrations and nothing else -- every device already registered keeps
	// reporting, and its SOS path is untouched.
	PlanExpiresAt *time.Time `json:"plan_expires_at,omitempty"`
	// KofiClaimCode is dead weight kept only so the column round-trips.
	// Nothing writes it: a payment is bound to a tenant by the metadata on its
	// Checkout session now (MESHSAT-1023). It used to be the short code a
	// supporter put in a message so
	// a payment can be matched to this tenant. People pay from a different
	// address than they signed up with often enough that email alone loses
	// payments. Never logged with the payment payload.
	KofiClaimCode string `json:"-"`
	// KofiPayerEmail is the address the last matched payment came from, learned
	// when a payment first matches this tenant.
	//
	// It exists because Ko-fi carries the supporter's message only on the join
	// payment; every renewal has message null. The claim code therefore matches
	// once and never again, so the payer has to be remembered or a paying
	// subscriber lapses at day 32 while their card is still being charged.
	KofiPayerEmail string `json:"-"`
	// KofiLastMessageID is the id of the last Ko-fi delivery applied to this
	// tenant. Ko-fi retries the same message_id until it gets a 200, so a
	// response lost on the way back would otherwise buy a second month for
	// free. Compared before a payment is applied.
	KofiLastMessageID string `json:"-"`
	// LapseWarnedAt is when the customer was last warned their plan is about to
	// end. It stops an hourly job from mailing hourly: a warning counts for the
	// expiry it was sent for, so a renewal that pushes the date out re-arms it.
	LapseWarnedAt *time.Time `json:"-"`

	// StripeCustomerID and StripeSubscriptionID bind a tenant to the payment
	// provider (MESHSAT-1023). The customer id is written once, at checkout,
	// from metadata this Hub put on the session -- which is why there is no
	// claim code any more: the payment is bound to a tenant by construction
	// rather than by somebody typing a code into a message box. The
	// subscription id is cleared when the subscription ends, so an empty one
	// means no live subscription rather than an unknown one.
	StripeCustomerID     string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string `json:"stripe_subscription_id,omitempty"`

	// BillingCountry is where the buyer is, ISO 3166-1 alpha-2, empty when
	// unknown. It decides whether Dutch VAT applies at all -- see internal/vat.
	// Before it existed every customer was written into the billing system as
	// Dutch and charged 21%, which is wrong for anybody outside the EU
	// (MESHSAT-1016).
	BillingCountry string `json:"billing_country,omitempty"`
	// BillingCountryEvidence records what the country was derived from. The
	// rules want evidence of a consumer's location rather than an assertion,
	// and "declared NL, seen from 1.2.3.4" is that evidence in the one place
	// somebody will look for it.
	BillingCountryEvidence string `json:"-"`
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

// ErrBillingCountry is returned by CreateTenant and UpdateTenant when
// Tenant.BillingCountry is neither empty nor an ISO 3166-1 alpha-2 code.
//
// The column is VARCHAR(2) in Postgres, so a longer value used to fail there
// as SQLSTATE 22001 at the bottom of a customer's first sign-in (MESHSAT-1365:
// "USA" from the enrolment form), while sqlite, which the unit tests run on,
// enforces no length at all and let the defect through. Both backends now
// refuse the same way, so the tests see what production sees, and the error
// names the field.
var ErrBillingCountry = errors.New("store: billing country must be empty or an ISO 3166-1 alpha-2 code")

// CheckBillingCountry is the guard every tenant writer applies before the
// row is touched.
func CheckBillingCountry(t *Tenant) error {
	if t.BillingCountry == "" || vat.IsAlpha2(t.BillingCountry) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrBillingCountry, t.BillingCountry)
}

// GeoPoint is one vertex of a geofence polygon.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// MeshNode is one Meshtastic node the Hub has HEARD behind one bridge.
//
// It exists because the Hub could not previously answer "is anything reachable
// behind this kit?" (MESHSAT-1181). Bridge liveness is a cellular fact and says
// nothing about radio: a kit can be perfectly online with an empty mesh, and a
// booth visitor who picks it gets silence.
//
// Read the asymmetry carefully, because the whole design turns on it. A row
// here is PROOF a node transmitted behind that bridge at LastHeard. The ABSENCE
// of a row proves nothing at all -- a node that is present and quiet is
// indistinguishable from one that is not there. Only mo/decoded carries a
// bridge_id, so a node becomes visible when a human sends text through it and
// not before; at the start of a show day the table is legitimately empty while
// both meshes are fine. Anything built on this must therefore treat presence as
// a positive signal to act on and silence as "unknown", never as "empty".
type MeshNode struct {
	TenantID   string
	BridgeID   string
	NodeID     string
	FirstHeard time.Time
	LastHeard  time.Time
}

// BoothSession is one visitor's position in the scripted booth menu.
//
// Keyed by (tenant, sender, channel) so the same phone number reaching us on
// WhatsApp and on SMS is two conversations, not one confused one.
type BoothSession struct {
	TenantID  string
	Sender    string
	Channel   string
	State     string
	OptedInAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BoothRelay is one message a visitor put on the mesh, and the correlation that
// lets the reply find its way back to them.
//
// This is in the database and not in a map because both Hub replicas process
// every message: the reply coming back off the mesh routinely arrives at the pod
// that did not send the original. In-memory state here would route correctly
// about half the time and the failure would look like the mesh losing messages.
//
// Ref is the short token carried in the mesh text ("#A7 ...", bare and at
// the start). The Bridge's SanitizeSMSText rewrites [ ] { } | \ ^ ~ on
// anything a kit sends to a plaintext peer, so a bracketed token survives
// inbound and is mangled coming back (MESHSAT-1178). ClosedAt nil
// means the conversation is still open, which is what makes the fallback
// possible: a reply with no ref can still be routed when its (bridge, node) has
// exactly one open conversation. Two open, and the Hub asks rather than guesses.
type BoothRelay struct {
	TenantID  string
	Ref       string
	Sender    string
	Channel   string
	BridgeID  string
	MeshDest  string
	Body      string
	CreatedAt time.Time
	ExpiresAt time.Time
	ClosedAt  *time.Time
}

// PersonConversation is one device's conversation with one person in the "@"
// lane (MESHSAT-1685): the device wrote "@+31612345678 ..." or
// "@ops@example.org ...", and the person's replies go back to it.
//
// In the database and not in a map for the booth's reason: both replicas
// process every message, and a reply lands on whichever pod Twilio or the
// mail service reached. Keyed (tenant_id, id), id derived from tenant, device,
// kind and person, so writing to the same person again is the same row.
//
// LastOutAt opens the reply window: a person's reply reaches the device only
// while the device has written to them recently, which is what keeps a
// stranger from spending the tenant's satellite credit. It moves only when a
// send was accepted (MarkPersonConversationOut); a row saved for a send that
// then failed keeps the zero time, so a mistyped number opens no window. Tag tells a person's
// conversations with several devices of one tenant apart on SMS; Ref is the
// reference an email reply must carry (a From line can be forged, the
// reference only the mailbox the Hub wrote to has).
type PersonConversation struct {
	TenantID   string
	ID         string
	DeviceID   string // a modem IMEI, or a kit's bridge id
	DeviceKind string // "satellite" or "kit"
	PeerKind   string // "sms" or "email"
	Peer       string // E.164, or the mail address as the device wrote it
	Tag        string
	Ref        string
	LastOutAt  time.Time
	LastInAt   *time.Time
	// The Message-IDs of the last mail each way, which thread the next one.
	LastOutMessageID string
	LastInMessageID  string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// PersonChatSend is one message the "@" lane put on a bearer, so a reader
// can see what was sent: "out" to a person, "in" to a device, "notice" to a
// device about something that could not be sent. Never a body. The ceilings
// are TakePersonChatSlot's buckets, which hold across both replicas without a
// count-then-insert race.
type PersonChatSend struct {
	TenantID       string
	ID             string
	ConversationID string
	Direction      string
	E2E            bool
	Bytes          int
	Status         string
	CreatedAt      time.Time
}

// TAKContact is one crew a client of the tenant puts on the TAK map
// (MESHSAT-1706): the uid and callsign it shows under, and the bridge (a kit,
// or a phone, which joins as a bridge) whose export last carried it. A chat
// addressed to the crew goes to that bridge alone.
type TAKContact struct {
	TenantID string    `json:"-"`
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	BridgeID string    `json:"bridge_id"`
	LastSeen time.Time `json:"last_seen"`
}

// TAKChatMessage is one message between an operator of the tenant and a crew,
// written from the Hub's map ("out") or answered by the crew ("in"). ID is the
// GeoChat's message id, so a message stored twice (both replicas, a resent
// request) is one row.
type TAKChatMessage struct {
	TenantID    string    `json:"-"`
	ID          string    `json:"id"`
	CrewUID     string    `json:"crew_uid"`
	OperatorUID string    `json:"operator_uid"`
	UserID      string    `json:"user_id,omitempty"`
	Direction   string    `json:"direction"`
	Text        string    `json:"text"`
	BridgeID    string    `json:"bridge_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// EmailContact is a correspondent's PGP public key, as stored.
type EmailContact struct {
	TenantID   string    `json:"tenant_id"`
	Email      string    `json:"email"`
	ArmoredKey string    `json:"armored_key"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Geofence is a persisted geofence. The geo package owns the geometry and the
// trigger semantics; this is only what goes in the table.
type Geofence struct {
	ID       string     `json:"id"`
	TenantID string     `json:"tenant_id"`
	Name     string     `json:"name"`
	Polygon  []GeoPoint `json:"polygon"`
	Trigger  string     `json:"trigger"`  // enter, exit, both
	ChainID  string     `json:"chain_id"` // escalation chain to raise
	// CooldownSec suppresses repeat events for the same device and fence after
	// one fires. 0 means the platform default. It damps the output, not the
	// input: the first crossing is never delayed (MESHSAT-1119).
	CooldownSec int       `json:"cooldown_sec,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

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
	// PlatformAdmin lets this key act across tenants, the axis that until
	// MESHSAT-1209 only the static HUB_AUTH_TOKEN could carry. Settable ONLY by a
	// caller who already holds it -- see internal/api/apikeys.go. Unlike that token
	// a key with this flag is revocable and can expire, which is the entire point.
	PlatformAdmin bool `json:"platform_admin,omitempty"`
	// PlatformRead lets this key READ the platform directory across tenants
	// (MESHSAT-1527): GET and HEAD on the routes behind auth.RequirePlatformRead,
	// nothing else. PlatformAdmin cannot be narrowed by the key's role --
	// RequirePlatformAdmin reads only the flag, so a "viewer" key carrying it can
	// still PUT and DELETE tenants -- which is why a read-only consumer (an
	// operator's lookup tool) gets this separate axis instead. Same gate as
	// PlatformAdmin: settable ONLY by a caller who holds PlatformAdmin.
	PlatformRead bool      `json:"platform_read,omitempty"`
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
// DonationPlan is the value receipts.plan carries for a one-off donation
// rather than a subscription.
//
// It lives here, in the lowest layer, because two queries have to recognise it
// in SQL -- CrossBorderSalesSince excludes it from the VAT threshold -- and a
// store cannot import internal/billing without a cycle. billing.DonationPlan
// aliases this so there is one spelling of the string and not two that agree
// today.
const DonationPlan = "donation"

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
	// Country is where the buyer was, ISO 3166-1 alpha-2, copied from the
	// tenant when the payment was recorded and frozen there. It decides the VAT
	// treatment (internal/vat), and it is frozen rather than re-read because a
	// customer who moves next year must not change the VAT on a document that
	// was already issued.
	Country string `json:"country,omitempty"`
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

	Status        string    `json:"status"` // ReceiptPending, ReceiptIssued, ReceiptBlocked
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	InvoiceNumber string    `json:"invoice_number,omitempty"`
	InvoiceRef    string    `json:"invoice_ref,omitempty"`
	// PaymentRef is the provider's payment reference (a Stripe payment
	// intent), learned AFTER the receipt is written and only so a refund can
	// find it again.
	//
	// It exists because the refund event no longer carries the invoice: Stripe
	// removed charge.invoice, and neither the charge nor the payment intent
	// links back to one. Without this a refunded subscription matched no
	// receipt, so no credit note was issued and the sale stayed in the books
	// at full value with its VAT declared.
	PaymentRef string     `json:"payment_ref,omitempty"`
	IssuedAt   *time.Time `json:"issued_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
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

// Refund is money given back that owes the customer a credit note
// (MESHSAT-1019).
//
// The terms promise EU consumers the 14-day right of withdrawal, and a
// payment is refundable in the payment processor. Until this existed the money
// went back and no document followed it from anywhere: not from the processor,
// whose refund emails are off by deliberate setting, and not from the billing
// system, which had no credit-note flow wired. The sale stayed in the books at
// full value with its VAT declared.
//
// The money itself moves by hand, in the payment processor, because that is
// where it lives. This row is the record that it moved and the instruction to
// produce the document, and it is written by an operator rather than inferred:
// a refund nobody recorded is a refund nobody can be held to.
type Refund struct {
	ID string `json:"id"`
	// TenantID is whose payment was given back. Tenant-scoped like the receipt
	// it reverses, so export and purge pick it up.
	TenantID string `json:"tenant_id"`
	// ReceiptID is the payment being reversed. UNIQUE: one refund per payment.
	// A partial refund is still one refund; recording a second one against the
	// same payment is refused rather than allowed to double-credit.
	ReceiptID string `json:"receipt_id"`
	// AmountCents is the GROSS amount given back, in the smallest unit of
	// Currency. It may be less than the payment, and never more -- the handler
	// refuses that rather than crediting money nobody paid.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// Country is copied from the receipt and frozen, for the same reason the
	// receipt freezes it: the credit note must reverse the VAT the invoice
	// charged, whatever the customer's address says today.
	Country string `json:"country,omitempty"`
	// Reason is why the money went back, in an operator's words. It goes on the
	// audit line and is not shown to the customer.
	Reason string `json:"reason,omitempty"`
	// RequestedBy names the operator who recorded it. An action on somebody
	// else's money with no name attached is only half a record.
	RequestedBy string `json:"requested_by,omitempty"`
	// RefundedAt is when the money actually went back, which dates the credit
	// note. It is the operator's figure, not the moment this row was written.
	RefundedAt time.Time `json:"refunded_at"`

	Status        string     `json:"status"` // RefundPending, RefundIssued, RefundBlocked
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt time.Time  `json:"next_attempt_at,omitempty"`
	CreditNumber  string     `json:"credit_number,omitempty"`
	CreditRef     string     `json:"credit_ref,omitempty"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Refund statuses.
const (
	// RefundPending means the credit note has not been produced yet. The
	// drainer keeps trying; nothing expires out of this state on its own.
	RefundPending = "pending"
	// RefundIssued means the billing system produced the credit note and the
	// customer has been sent it. CreditNumber says which one.
	//
	// It also covers the case where nothing was owed: a payment refunded
	// before its receipt was ever issued has no invoice to credit, so the
	// receipt is cancelled instead and this row is closed with no number. That
	// is a complete outcome, not a failure.
	RefundIssued = "issued"
	// RefundBlocked means a person has to look. The commonest cause is a
	// receipt whose invoice was deleted behind our back.
	RefundBlocked = "blocked"
)
