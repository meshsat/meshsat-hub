package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Bridges, bridge MQTT credentials and HeMB bond groups. Semantics follow
// internal/store/mariadb exactly (same not-found errors, ordering, tenant
// scoping, and the same set of columns a birth upsert is allowed to touch).

// defaultJSON returns fallback when s is empty: JSONB rejects an empty string.
func defaultJSON(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// bridgeColumns is the projection every bridge read shares. withSecret adds
// mqtt_password_hash (only ListBridgesWithCredentials exposes it).
func bridgeColumns(withSecret bool) string {
	secret := ""
	if withSecret {
		secret = "mqtt_password_hash, "
	}
	return `bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen, last_report_bearer, last_report_at,
			mqtt_username, ` + secret + `cert_pem, cert_expiry,
			created_at, updated_at`
}

func scanBridge(s rowScanner, withSecret bool) (*store.Bridge, error) {
	var b store.Bridge
	var lastSeen, lastReportAt, certExpiry sql.NullTime
	var createdAt, updatedAt time.Time
	dest := []any{&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
		&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
		&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
		&b.Online, &b.LastBirth, &b.LastHealth, &lastSeen, &b.LastReportBearer, &lastReportAt,
		&b.MQTTUsername}
	if withSecret {
		dest = append(dest, &b.MQTTPasswordHash)
	}
	dest = append(dest, &b.CertPEM, &certExpiry, &createdAt, &updatedAt)
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}
	b.LastSeen = utcPtr(lastSeen)
	b.LastReportAt = utcPtr(lastReportAt)
	b.CertExpiry = utcPtr(certExpiry)
	b.CreatedAt = utc(createdAt)
	b.UpdatedAt = utc(updatedAt)
	return &b, nil
}

func (d *DB) queryBridges(ctx context.Context, withSecret bool, where string, args ...any) ([]*store.Bridge, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+bridgeColumns(withSecret)+" FROM bridges "+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var bridges []*store.Bridge
	for rows.Next() {
		b, err := scanBridge(rows, withSecret)
		if err != nil {
			return nil, err
		}
		bridges = append(bridges, b)
	}
	return bridges, rows.Err()
}

// CreateOrUpdateBridge upserts the birth-provided fields. On conflict it does
// NOT touch mqtt_username, mqtt_password_hash, cert_pem, cert_expiry or
// created_at: a bridge re-announcing itself must never lose its credentials.
func (d *DB) CreateOrUpdateBridge(ctx context.Context, tenantID string, b *store.Bridge) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO bridges (bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, now())
		 ON CONFLICT (bridge_id) DO UPDATE SET
			tenant_id=EXCLUDED.tenant_id, label=EXCLUDED.label, hostname=EXCLUDED.hostname,
			version=EXCLUDED.version, mode=EXCLUDED.mode,
			location_lat=EXCLUDED.location_lat, location_lon=EXCLUDED.location_lon,
			location_alt=EXCLUDED.location_alt, capabilities=EXCLUDED.capabilities,
			reticulum_hash=EXCLUDED.reticulum_hash, reticulum_pubkey=EXCLUDED.reticulum_pubkey,
			cot_type=EXCLUDED.cot_type, cot_callsign=EXCLUDED.cot_callsign,
			online=EXCLUDED.online, last_birth=EXCLUDED.last_birth, last_health=EXCLUDED.last_health,
			last_seen=now(), updated_at=now()`,
		b.BridgeID, tenantID, b.Label, b.Hostname, b.Version, b.Mode,
		b.LocationLat, b.LocationLon, b.LocationAlt, defaultJSON(b.Capabilities, "[]"),
		b.ReticulumHash, b.ReticulumPubkey, b.CoTType, b.CoTCallsign,
		b.Online, defaultJSON(b.LastBirth, "{}"), defaultJSON(b.LastHealth, "{}"))
	return err
}

func (d *DB) GetBridge(ctx context.Context, tenantID string, bridgeID string) (*store.Bridge, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+bridgeColumns(false)+" FROM bridges WHERE bridge_id=$1 AND tenant_id=$2", bridgeID, tenantID)
	return scanBridge(row, false)
}

func (d *DB) ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error) {
	return d.queryBridges(ctx, false, "WHERE tenant_id=$1 ORDER BY label, bridge_id", tenantID)
}

func (d *DB) UpdateBridge(ctx context.Context, tenantID string, bridgeID string, updates store.BridgeUpdate) error {
	setClauses := "updated_at=now()"
	args := []any{}
	if updates.Label != nil {
		args = append(args, *updates.Label)
		setClauses += fmt.Sprintf(", label=$%d", len(args))
	}
	if updates.CoTCallsign != nil {
		args = append(args, *updates.CoTCallsign)
		setClauses += fmt.Sprintf(", cot_callsign=$%d", len(args))
	}
	args = append(args, bridgeID, tenantID)
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf("UPDATE bridges SET %s WHERE bridge_id=$%d AND tenant_id=$%d", setClauses, len(args)-1, len(args)),
		args...)
	return err
}

func (d *DB) DeleteBridge(ctx context.Context, tenantID string, bridgeID string) error {
	_, _ = d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=NULL WHERE bridge_id=$1 AND tenant_id=$2", bridgeID, tenantID)
	_, err := d.db.ExecContext(ctx, "DELETE FROM bridges WHERE bridge_id=$1 AND tenant_id=$2", bridgeID, tenantID)
	return err
}

func (d *DB) SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET online=$1, updated_at=now() WHERE bridge_id=$2 AND tenant_id=$3",
		online, bridgeID, tenantID)
	return err
}

func (d *DB) SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_report_bearer=$1, last_report_at=$2, updated_at=now() WHERE bridge_id=$3 AND tenant_id=$4",
		bearer, at.UTC(), bridgeID, tenantID)
	return err
}

func (d *DB) TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_seen=now() WHERE bridge_id=$1 AND tenant_id=$2", bridgeID, tenantID)
	return err
}

// MarkStaleBridgesOffline flips online bridges whose last_seen is older than
// timeout to offline and returns the number of rows changed (all tenants).
func (d *DB) MarkStaleBridgesOffline(ctx context.Context, timeout time.Duration) (int64, error) {
	secs := int(timeout.Seconds())
	res, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET online=FALSE, updated_at=now()
		 WHERE online=TRUE AND last_seen IS NOT NULL AND last_seen < now() - ($1::int * interval '1 second')`,
		secs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_health=$1, updated_at=now() WHERE bridge_id=$2 AND tenant_id=$3",
		defaultJSON(health, "{}"), bridgeID, tenantID)
	return err
}

func (d *DB) AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=$1 WHERE imei=$2 AND tenant_id=$3", bridgeID, imei, tenantID)
	return err
}

// --- Bridge MQTT credentials ---

func (d *DB) SetBridgeCredentials(ctx context.Context, tenantID, bridgeID, username, passwordHash string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET mqtt_username=$1, mqtt_password_hash=$2, updated_at=now()
		 WHERE bridge_id=$3 AND tenant_id=$4`,
		username, passwordHash, bridgeID, tenantID)
	return err
}

func (d *DB) GetBridgeCredentials(ctx context.Context, tenantID, bridgeID string) (*store.BridgeCredentials, error) {
	var c store.BridgeCredentials
	var certExpiry sql.NullTime
	var createdAt time.Time
	err := d.db.QueryRowContext(ctx,
		`SELECT bridge_id, mqtt_username, mqtt_password_hash, cert_pem, cert_expiry, created_at
		 FROM bridges WHERE bridge_id=$1 AND tenant_id=$2`, bridgeID, tenantID,
	).Scan(&c.BridgeID, &c.Username, &c.Password, &c.CertPEM, &certExpiry, &createdAt)
	if err != nil {
		return nil, err
	}
	c.CertExpiry = utcPtr(certExpiry)
	c.CreatedAt = utc(createdAt)
	return &c, nil
}

func (d *DB) SetBridgeCertificate(ctx context.Context, tenantID, bridgeID, certPEM string, expiry time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET cert_pem=$1, cert_expiry=$2, updated_at=now()
		 WHERE bridge_id=$3 AND tenant_id=$4`,
		certPEM, expiry.UTC(), bridgeID, tenantID)
	return err
}

// ListBridgesWithCredentials returns every bridge (all tenants) that has an
// MQTT username, including the password hash, for ACL/auth file generation.
func (d *DB) ListBridgesWithCredentials(ctx context.Context) ([]*store.Bridge, error) {
	return d.queryBridges(ctx, true, "WHERE mqtt_username <> '' ORDER BY bridge_id")
}

// --- HeMB bond groups (MESHSAT-429) ---

const bondGroupColumns = "id, tenant_id, bridge_id, label, members, cost_budget, created_at"

// scanBondGroup formats created_at the way database/sql does when the MariaDB
// driver hands a time.Time to the struct's string field: RFC3339Nano in UTC.
func scanBondGroup(s rowScanner) (*store.BondGroup, error) {
	var g store.BondGroup
	var createdAt time.Time
	if err := s.Scan(&g.ID, &g.TenantID, &g.BridgeID, &g.Label, &g.Members, &g.CostBudget, &createdAt); err != nil {
		return nil, err
	}
	g.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	return &g, nil
}

func (d *DB) CreateBondGroup(ctx context.Context, tenantID, bridgeID string, g *store.BondGroup) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO bond_groups (id, tenant_id, bridge_id, label, members, cost_budget) VALUES ($1, $2, $3, $4, $5, $6)",
		g.ID, tenantID, bridgeID, g.Label, g.Members, g.CostBudget)
	return err
}

func (d *DB) GetBondGroups(ctx context.Context, tenantID, bridgeID string) ([]store.BondGroup, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+bondGroupColumns+" FROM bond_groups WHERE tenant_id=$1 AND bridge_id=$2 ORDER BY created_at ASC",
		tenantID, bridgeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var groups []store.BondGroup
	for rows.Next() {
		g, err := scanBondGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, *g)
	}
	return groups, rows.Err()
}

func (d *DB) GetBondGroup(ctx context.Context, tenantID, bridgeID, groupID string) (*store.BondGroup, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+bondGroupColumns+" FROM bond_groups WHERE tenant_id=$1 AND bridge_id=$2 AND id=$3",
		tenantID, bridgeID, groupID)
	return scanBondGroup(row)
}

func (d *DB) UpdateBondGroup(ctx context.Context, tenantID, bridgeID string, g *store.BondGroup) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bond_groups SET label=$1, members=$2, cost_budget=$3 WHERE tenant_id=$4 AND bridge_id=$5 AND id=$6",
		g.Label, g.Members, g.CostBudget, tenantID, bridgeID, g.ID)
	return err
}

func (d *DB) DeleteBondGroup(ctx context.Context, tenantID, bridgeID, groupID string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM bond_groups WHERE tenant_id=$1 AND bridge_id=$2 AND id=$3",
		tenantID, bridgeID, groupID)
	return err
}
