package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Bridges and bridge MQTT credentials. Semantics follow
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
// It deliberately does NOT move an existing bridge between tenants: this runs
// from an MQTT birth, and a bridge that could rewrite its own tenant_id could
// walk itself into somebody else's fleet and out of its own tenant's quota.
// Ownership changes through the API, where a human is asking.
//
// The row, the birth's declarations and its modem links are written in one
// transaction, each part only where it is not older than what is stored
// (store.Bridge.BirthStamp, Egress, Links; HeMB-NG S3-2). Every conditional
// write is an upsert or update that locks the row it compares against, so two
// replicas writing births of one bridge are ordered by the stamps, not by
// which commits last.
func (d *DB) CreateOrUpdateBridge(ctx context.Context, tenantID string, b *store.Bridge) error {
	tx, err := d.rawDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO bridges (bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, birth_stamp, last_seen)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, now())
		 ON CONFLICT (bridge_id) DO UPDATE SET
			label=EXCLUDED.label, hostname=EXCLUDED.hostname,
			version=EXCLUDED.version, mode=EXCLUDED.mode,
			location_lat=EXCLUDED.location_lat, location_lon=EXCLUDED.location_lon,
			location_alt=EXCLUDED.location_alt, capabilities=EXCLUDED.capabilities,
			reticulum_hash=EXCLUDED.reticulum_hash, reticulum_pubkey=EXCLUDED.reticulum_pubkey,
			cot_type=EXCLUDED.cot_type, cot_callsign=EXCLUDED.cot_callsign,
			online=EXCLUDED.online, last_birth=EXCLUDED.last_birth, last_health=EXCLUDED.last_health,
			birth_stamp=EXCLUDED.birth_stamp,
			last_seen=now(), updated_at=now()
		 WHERE bridges.tenant_id = EXCLUDED.tenant_id AND EXCLUDED.birth_stamp >= bridges.birth_stamp`,
		b.BridgeID, tenantID, b.Label, b.Hostname, b.Version, b.Mode,
		b.LocationLat, b.LocationLon, b.LocationAlt, defaultJSON(b.Capabilities, "[]"),
		b.ReticulumHash, b.ReticulumPubkey, b.CoTType, b.CoTCallsign,
		b.Online, defaultJSON(b.LastBirth, "{}"), defaultJSON(b.LastHealth, "{}"), b.BirthStamp)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// The conflict update is guarded on tenant_id (MESHSAT-1307): without
		// it, a second tenant naming an existing bridge id rewrote the other
		// tenant's row. Zero rows means that, refused; or a row that holds a
		// newer birth, kept.
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT tenant_id FROM bridges WHERE bridge_id=$1", b.BridgeID).Scan(&owner); err != nil {
			return err
		}
		if owner != tenantID {
			return store.ErrOwnedElsewhere
		}
	}
	for imei, e := range b.Egress {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bridge_egress (tenant_id, bridge_id, imei, format, stamp, declared_at)
			 VALUES ($1, $2, $3, $4, $5, now())
			 ON CONFLICT (tenant_id, bridge_id, imei) DO UPDATE SET
				format=EXCLUDED.format, stamp=EXCLUDED.stamp, declared_at=EXCLUDED.declared_at
			 WHERE EXCLUDED.stamp >= bridge_egress.stamp`,
			tenantID, b.BridgeID, imei, e.Format, e.Stamp); err != nil {
			return err
		}
	}
	for imei, at := range b.Links {
		if _, err := tx.ExecContext(ctx,
			"UPDATE devices SET bridge_id=$1, bridge_linked_at=$2 WHERE imei=$3 AND tenant_id=$4 AND bridge_linked_at <= $2",
			b.BridgeID, at, imei, tenantID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		"UPDATE devices SET bridge_id=NULL, bridge_linked_at=$1 WHERE bridge_id=$2 AND tenant_id=$3",
		time.Now().UnixNano(), bridgeID, tenantID)
	_, _ = d.db.ExecContext(ctx, "DELETE FROM bridge_egress WHERE bridge_id=$1 AND tenant_id=$2", bridgeID, tenantID)
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
// MarkStaleBridgesOffline marks every bridge offline whose last_seen is older
// than ITS OWN TENANT's timeout, falling back to the platform default.
//
// One statement rather than a query per tenant (MESHSAT-1117): the reaper runs
// on a timer for the whole fleet, and a loop over tenants would turn a periodic
// UPDATE into N round trips that grow with the customer list. The subquery is a
// correlated lookup on the tenants primary key.
//
// COALESCE(NULLIF(..., 0), $1) is doing two jobs: NULLIF turns the sentinel 0
// ("this tenant has chosen nothing") into NULL, and COALESCE also covers a
// bridge whose tenant row is missing entirely -- which an inner join would have
// silently skipped, leaving that bridge online forever.
func (d *DB) MarkStaleBridgesOffline(ctx context.Context, timeout time.Duration) (int64, error) {
	secs := int(timeout.Seconds())
	res, err := d.db.ExecContext(ctx,
		`UPDATE bridges b SET online=FALSE, updated_at=now()
		 WHERE b.online=TRUE AND b.last_seen IS NOT NULL
		   AND b.last_seen < now() - (COALESCE(NULLIF(
		         (SELECT t.bridge_offline_timeout FROM tenants t WHERE t.id = b.tenant_id), 0
		       ), $1::int) * interval '1 second')`,
		secs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) RecordBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string, bearer string, at time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET last_health=$1, last_seen=now(), last_report_bearer=$2, last_report_at=$3,
		        online=TRUE, updated_at=now()
		 WHERE bridge_id=$4 AND tenant_id=$5`,
		defaultJSON(health, "{}"), bearer, at.UTC(), bridgeID, tenantID)
	return err
}

func (d *DB) SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_health=$1, updated_at=now() WHERE bridge_id=$2 AND tenant_id=$3",
		defaultJSON(health, "{}"), bridgeID, tenantID)
	return err
}

func (d *DB) AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=$1, bridge_linked_at=$2 WHERE imei=$3 AND tenant_id=$4",
		bridgeID, time.Now().UnixNano(), imei, tenantID)
	return err
}

func (d *DB) DeviceBridgeID(ctx context.Context, tenantID string, imei string) (string, error) {
	var id string
	err := d.db.QueryRowContext(ctx,
		"SELECT COALESCE(bridge_id, '') FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (d *DB) SetDeviceEgress(ctx context.Context, tenantID, imei, bridgeID, format string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE devices SET
			egress_format=COALESCE((SELECT format FROM bridge_egress WHERE tenant_id=$1 AND bridge_id=$2 AND imei=$3), $4),
			egress_bridge_id=$2, egress_declared_at=now()
		 WHERE imei=$3 AND tenant_id=$1`, tenantID, bridgeID, imei, format)
	return err
}

func (d *DB) DeviceEgress(ctx context.Context, tenantID, imei string) (string, string, error) {
	var bridgeID, format string
	err := d.db.QueryRowContext(ctx,
		"SELECT egress_bridge_id, egress_format FROM devices WHERE imei=$1 AND tenant_id=$2", imei, tenantID).Scan(&bridgeID, &format)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return bridgeID, format, err
}

func (d *DB) BridgeEgress(ctx context.Context, tenantID, bridgeID, imei string) (string, error) {
	e, err := d.BridgeEgressEntry(ctx, tenantID, bridgeID, imei)
	return e.Format, err
}

func (d *DB) BridgeEgressEntry(ctx context.Context, tenantID, bridgeID, imei string) (store.EgressDecl, error) {
	var e store.EgressDecl
	err := d.db.QueryRowContext(ctx,
		"SELECT format, stamp FROM bridge_egress WHERE tenant_id=$1 AND bridge_id=$2 AND imei=$3",
		tenantID, bridgeID, imei).Scan(&e.Format, &e.Stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return store.EgressDecl{}, nil
	}
	return e, err
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
