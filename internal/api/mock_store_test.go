package api

import (
	"context"
	"fmt"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// mockStore implements store.Store for unit tests with configurable return values.
type mockStore struct {
	// Devices
	devices     []store.Device
	device      *store.Device
	deviceErr   error
	createDevFn func(ctx context.Context, tid string, d *store.Device) error

	// Messages
	messages   []store.Message
	message    *store.Message
	messageErr error

	// Positions
	positions   []store.Position
	position    *store.Position
	positionErr error

	// Audit
	auditEntries []store.AuditEntry
	auditEntry   *store.AuditEntry
	auditErr     error

	// Device config
	deviceConfig  *store.DeviceConfig
	deviceConfigs []store.DeviceConfig
	configErr     error
	createCfgFn   func(ctx context.Context, tid string, c *store.DeviceConfig) error

	// API keys
	apiKeys     []store.APIKey
	apiKey      *store.APIKey
	apiKeyErr   error
	createKeyFn func(ctx context.Context, tid string, k *store.APIKey) error
}

func (m *mockStore) Migrate(context.Context) error { return nil }
func (m *mockStore) Close() error                  { return nil }
func (m *mockStore) Ping(context.Context) error    { return nil }

func (m *mockStore) CreateDevice(ctx context.Context, tid string, d *store.Device) error {
	if m.createDevFn != nil {
		return m.createDevFn(ctx, tid, d)
	}
	return m.deviceErr
}
func (m *mockStore) GetDevice(_ context.Context, _ string, _ string) (*store.Device, error) {
	if m.device == nil && m.deviceErr == nil {
		return nil, fmt.Errorf("not found")
	}
	return m.device, m.deviceErr
}
func (m *mockStore) ListDevices(_ context.Context, _ string) ([]store.Device, error) {
	return m.devices, m.deviceErr
}
func (m *mockStore) UpdateDevice(_ context.Context, _ string, _ *store.Device) error {
	return m.deviceErr
}
func (m *mockStore) DeleteDevice(_ context.Context, _ string, _ string) error {
	return m.deviceErr
}
func (m *mockStore) TouchDeviceLastSeen(context.Context, string, string) error { return nil }

func (m *mockStore) InsertMessage(context.Context, string, *store.Message) error { return nil }
func (m *mockStore) ListMessages(_ context.Context, _ string, _ string, _ int) ([]store.Message, error) {
	return m.messages, m.messageErr
}
func (m *mockStore) GetMessage(_ context.Context, _ string, _ string) (*store.Message, error) {
	if m.message == nil && m.messageErr == nil {
		return nil, fmt.Errorf("not found")
	}
	return m.message, m.messageErr
}

func (m *mockStore) SaveWebhook(context.Context, string, *store.WebhookConfig) error { return nil }
func (m *mockStore) ListWebhooks(context.Context, string) ([]store.WebhookConfig, error) {
	return nil, nil
}
func (m *mockStore) DeleteWebhook(context.Context, string, string) error                 { return nil }
func (m *mockStore) InsertDeliveryLog(context.Context, string, *store.DeliveryLog) error { return nil }
func (m *mockStore) ListDeliveryLogs(context.Context, string, int) ([]store.DeliveryLog, error) {
	return nil, nil
}

func (m *mockStore) InsertPosition(context.Context, string, *store.Position) error { return nil }
func (m *mockStore) LatestPosition(_ context.Context, _ string, _ string) (*store.Position, error) {
	if m.position == nil && m.positionErr == nil {
		return nil, fmt.Errorf("not found")
	}
	return m.position, m.positionErr
}
func (m *mockStore) ListPositions(_ context.Context, _ string, _ string, _ int) ([]store.Position, error) {
	return m.positions, m.positionErr
}
func (m *mockStore) ListPositionsRange(_ context.Context, _ string, _ string, _, _ time.Time, _, _ int) ([]store.Position, int, error) {
	return m.positions, len(m.positions), m.positionErr
}

func (m *mockStore) InsertAuditEntry(context.Context, string, *store.AuditEntry) error { return nil }
func (m *mockStore) ListAuditEntries(_ context.Context, _ string, _ int) ([]store.AuditEntry, error) {
	return m.auditEntries, m.auditErr
}
func (m *mockStore) GetLatestAuditEntry(_ context.Context, _ string) (*store.AuditEntry, error) {
	return m.auditEntry, m.auditErr
}

func (m *mockStore) CreateDeviceConfig(ctx context.Context, tid string, c *store.DeviceConfig) error {
	if m.createCfgFn != nil {
		return m.createCfgFn(ctx, tid, c)
	}
	return m.configErr
}
func (m *mockStore) GetDeviceConfigLatest(_ context.Context, _ string, _ string) (*store.DeviceConfig, error) {
	return m.deviceConfig, m.configErr
}
func (m *mockStore) GetDeviceConfigVersion(_ context.Context, _ string, _ string, _ int) (*store.DeviceConfig, error) {
	return m.deviceConfig, m.configErr
}
func (m *mockStore) ListDeviceConfigVersions(_ context.Context, _ string, _ string, _ int) ([]store.DeviceConfig, error) {
	return m.deviceConfigs, m.configErr
}

func (m *mockStore) CreateAPIKey(ctx context.Context, tid string, k *store.APIKey) error {
	if m.createKeyFn != nil {
		return m.createKeyFn(ctx, tid, k)
	}
	return m.apiKeyErr
}
func (m *mockStore) GetAPIKeyByHash(context.Context, string) (*store.APIKey, string, error) {
	return m.apiKey, "test-tenant", m.apiKeyErr
}
func (m *mockStore) ListAPIKeys(_ context.Context, _ string) ([]store.APIKey, error) {
	return m.apiKeys, m.apiKeyErr
}
func (m *mockStore) DeleteAPIKey(_ context.Context, _ string, _ string) error {
	return m.apiKeyErr
}
func (m *mockStore) TouchAPIKeyLastUsed(context.Context, string) error { return nil }

func (m *mockStore) CreateEscalationChain(context.Context, string, *store.EscalationChain) error {
	return nil
}
func (m *mockStore) GetEscalationChain(context.Context, string, string) (*store.EscalationChain, error) {
	return nil, nil
}
func (m *mockStore) ListEscalationChains(context.Context, string) ([]store.EscalationChain, error) {
	return nil, nil
}
func (m *mockStore) DeleteEscalationChain(context.Context, string, string) error { return nil }
func (m *mockStore) CreateAlert(context.Context, string, *store.Alert) error     { return nil }
func (m *mockStore) GetAlert(context.Context, string, string) (*store.Alert, error) {
	return nil, nil
}
func (m *mockStore) ListAlerts(context.Context, string, bool, int) ([]store.Alert, error) {
	return nil, nil
}
func (m *mockStore) UpdateAlert(context.Context, string, *store.Alert) error { return nil }

func (m *mockStore) SaveNotificationPref(context.Context, string, *store.NotificationPref) error {
	return nil
}
func (m *mockStore) GetNotificationPref(context.Context, string, string) (*store.NotificationPref, error) {
	return nil, nil
}
func (m *mockStore) ListNotificationPrefs(context.Context, string) ([]store.NotificationPref, error) {
	return nil, nil
}
func (m *mockStore) DeleteNotificationPref(context.Context, string, string) error { return nil }

// Users (local accounts)
func (m *mockStore) CreateUser(context.Context, string, *store.LocalUser) error { return nil }
func (m *mockStore) GetUserByID(context.Context, string, string) (*store.LocalUser, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) GetUserByEmail(context.Context, string, string) (*store.LocalUser, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) ListUsers(context.Context, string) ([]store.LocalUser, error) { return nil, nil }
func (m *mockStore) UpdateUser(context.Context, string, *store.LocalUser) error   { return nil }
func (m *mockStore) DeleteUser(context.Context, string, string) error             { return nil }
func (m *mockStore) IncrementFailedLogins(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *mockStore) ResetFailedLogins(context.Context, string, string) error { return nil }

// Refresh tokens
func (m *mockStore) StoreRefreshToken(context.Context, string, *store.RefreshToken) error { return nil }
func (m *mockStore) GetRefreshToken(context.Context, string) (*store.RefreshToken, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) DeleteRefreshToken(context.Context, string) error                { return nil }
func (m *mockStore) DeleteRefreshTokensByUser(context.Context, string, string) error { return nil }

// Device encryption keys
func (m *mockStore) CreateDeviceKey(context.Context, string, *store.DeviceKey) error { return nil }
func (m *mockStore) ListDeviceKeys(context.Context, string, string) ([]store.DeviceKey, error) {
	return nil, nil
}
func (m *mockStore) GetDeviceKeyLatest(context.Context, string, string) (*store.DeviceKey, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) DeleteDeviceKey(context.Context, string, string) error { return nil }

func (m *mockStore) SaveDeviceWireguard(context.Context, string, *store.DeviceWireguard) error {
	return nil
}
func (m *mockStore) GetDeviceWireguard(context.Context, string, string) (*store.DeviceWireguard, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) DeleteDeviceWireguard(context.Context, string, string) error { return nil }

// Routes
func (m *mockStore) CreateRoute(context.Context, string, *store.Route) error { return nil }
func (m *mockStore) GetRoute(context.Context, string, string) (*store.Route, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) ListRoutes(context.Context, string) ([]store.Route, error) { return nil, nil }
func (m *mockStore) UpdateRoute(context.Context, string, *store.Route) error   { return nil }
func (m *mockStore) DeleteRoute(context.Context, string, string) error         { return nil }

// Bridges
func (m *mockStore) CreateOrUpdateBridge(context.Context, string, *store.Bridge) error { return nil }
func (m *mockStore) GetBridge(context.Context, string, string) (*store.Bridge, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) ListBridges(context.Context, string) ([]*store.Bridge, error) { return nil, nil }
func (m *mockStore) UpdateBridge(context.Context, string, string, store.BridgeUpdate) error {
	return nil
}
func (m *mockStore) DeleteBridge(context.Context, string, string) error            { return nil }
func (m *mockStore) SetBridgeOnline(context.Context, string, string, bool) error   { return nil }
func (m *mockStore) TouchBridgeLastSeen(context.Context, string, string) error     { return nil }
func (m *mockStore) SetBridgeHealth(context.Context, string, string, string) error { return nil }
func (m *mockStore) AssociateDeviceWithBridge(context.Context, string, string, string) error {
	return nil
}

// Bridge MQTT credentials
func (m *mockStore) SetBridgeCredentials(context.Context, string, string, string, string) error {
	return nil
}
func (m *mockStore) GetBridgeCredentials(context.Context, string, string) (*store.BridgeCredentials, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) SetBridgeCertificate(context.Context, string, string, string, time.Time) error {
	return nil
}
func (m *mockStore) ListBridgesWithCredentials(context.Context) ([]*store.Bridge, error) {
	return nil, nil
}

// Cost ledger
func (m *mockStore) InsertCostEntry(context.Context, string, *store.CostEntry) error { return nil }
func (m *mockStore) ListCostEntries(context.Context, string, string, time.Time, time.Time, int) ([]store.CostEntry, error) {
	return nil, nil
}
func (m *mockStore) AggregateCosts(context.Context, string, time.Time, time.Time, string) ([]store.CostAggregate, error) {
	return nil, nil
}

// System config
func (m *mockStore) GetSystemConfig(context.Context, string) (string, error) { return "", nil }
func (m *mockStore) SetSystemConfig(context.Context, string, string) error   { return nil }

// Device groups
func (m *mockStore) CreateDeviceGroup(context.Context, string, *store.DeviceGroup) error { return nil }
func (m *mockStore) GetDeviceGroup(context.Context, string, string) (*store.DeviceGroup, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) ListDeviceGroups(context.Context, string) ([]store.DeviceGroup, error) {
	return nil, nil
}
func (m *mockStore) UpdateDeviceGroup(context.Context, string, *store.DeviceGroup) error { return nil }
func (m *mockStore) DeleteDeviceGroup(context.Context, string, string) error             { return nil }
func (m *mockStore) AddDeviceToGroup(context.Context, string, string, string) error      { return nil }
func (m *mockStore) RemoveDeviceFromGroup(context.Context, string, string, string) error { return nil }
func (m *mockStore) ListDevicesInGroup(context.Context, string, string) ([]store.Device, error) {
	return nil, nil
}
func (m *mockStore) ListGroupsForDevice(context.Context, string, string) ([]store.DeviceGroup, error) {
	return nil, nil
}

// Message templates
func (m *mockStore) CreateMessageTemplate(context.Context, string, *store.MessageTemplate) error {
	return nil
}
func (m *mockStore) GetMessageTemplate(context.Context, string, string) (*store.MessageTemplate, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockStore) ListMessageTemplates(context.Context, string) ([]store.MessageTemplate, error) {
	return nil, nil
}
func (m *mockStore) UpdateMessageTemplate(context.Context, string, *store.MessageTemplate) error {
	return nil
}
func (m *mockStore) DeleteMessageTemplate(context.Context, string, string) error { return nil }
