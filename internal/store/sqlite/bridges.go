package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// --- Bridges ---

func (d *DB) CreateOrUpdateBridge(ctx context.Context, tenantID string, b *store.Bridge) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO bridges (bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(bridge_id) DO UPDATE SET
			tenant_id=excluded.tenant_id, label=excluded.label, hostname=excluded.hostname,
			version=excluded.version, mode=excluded.mode,
			location_lat=excluded.location_lat, location_lon=excluded.location_lon,
			location_alt=excluded.location_alt, capabilities=excluded.capabilities,
			reticulum_hash=excluded.reticulum_hash, reticulum_pubkey=excluded.reticulum_pubkey,
			cot_type=excluded.cot_type, cot_callsign=excluded.cot_callsign,
			online=excluded.online, last_birth=excluded.last_birth, last_health=excluded.last_health,
			last_seen=datetime('now'), updated_at=datetime('now')`,
		b.BridgeID, tenantID, b.Label, b.Hostname, b.Version, b.Mode,
		b.LocationLat, b.LocationLon, b.LocationAlt, b.Capabilities,
		b.ReticulumHash, b.ReticulumPubkey, b.CoTType, b.CoTCallsign,
		boolToInt(b.Online), b.LastBirth, b.LastHealth)
	return err
}

func (d *DB) GetBridge(ctx context.Context, tenantID string, bridgeID string) (*store.Bridge, error) {
	var b store.Bridge
	var online int
	var lastSeen, createdAt, updatedAt sql.NullString
	var certExpiry sql.NullString
	err := d.db.QueryRowContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen,
			mqtt_username, cert_pem, cert_expiry,
			created_at, updated_at
		 FROM bridges WHERE bridge_id=? AND tenant_id=?`, bridgeID, tenantID,
	).Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
		&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
		&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
		&online, &b.LastBirth, &b.LastHealth, &lastSeen,
		&b.MQTTUsername, &b.CertPEM, &certExpiry,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	b.Online = online != 0
	if lastSeen.Valid {
		t, _ := time.Parse(time.DateTime, lastSeen.String)
		b.LastSeen = &t
	}
	if certExpiry.Valid {
		t, _ := time.Parse(time.DateTime, certExpiry.String)
		b.CertExpiry = &t
	}
	if createdAt.Valid {
		b.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
	}
	if updatedAt.Valid {
		b.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
	}
	return &b, nil
}

func (d *DB) ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen,
			mqtt_username, cert_pem, cert_expiry,
			created_at, updated_at
		 FROM bridges WHERE tenant_id=? ORDER BY label, bridge_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var bridges []*store.Bridge
	for rows.Next() {
		var b store.Bridge
		var online int
		var lastSeen, certExpiry, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
			&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
			&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
			&online, &b.LastBirth, &b.LastHealth, &lastSeen,
			&b.MQTTUsername, &b.CertPEM, &certExpiry,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		b.Online = online != 0
		if lastSeen.Valid {
			t, _ := time.Parse(time.DateTime, lastSeen.String)
			b.LastSeen = &t
		}
		if certExpiry.Valid {
			t, _ := time.Parse(time.DateTime, certExpiry.String)
			b.CertExpiry = &t
		}
		if createdAt.Valid {
			b.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
		}
		if updatedAt.Valid {
			b.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
		}
		bridges = append(bridges, &b)
	}
	return bridges, nil
}

func (d *DB) UpdateBridge(ctx context.Context, tenantID string, bridgeID string, updates store.BridgeUpdate) error {
	setClauses := []string{"updated_at=datetime('now')"}
	args := []interface{}{}
	if updates.Label != nil {
		setClauses = append(setClauses, "label=?")
		args = append(args, *updates.Label)
	}
	if updates.CoTCallsign != nil {
		setClauses = append(setClauses, "cot_callsign=?")
		args = append(args, *updates.CoTCallsign)
	}
	args = append(args, bridgeID, tenantID)
	query := fmt.Sprintf("UPDATE bridges SET %s WHERE bridge_id=? AND tenant_id=?",
		joinStrings(setClauses, ", "))
	_, err := d.db.ExecContext(ctx, query, args...)
	return err
}

func (d *DB) DeleteBridge(ctx context.Context, tenantID string, bridgeID string) error {
	// Clear bridge_id on associated devices first.
	_, _ = d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=NULL WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	_, err := d.db.ExecContext(ctx, "DELETE FROM bridges WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	return err
}

func (d *DB) SetBridgeOnline(ctx context.Context, tenantID string, bridgeID string, online bool) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET online=?, updated_at=datetime('now') WHERE bridge_id=? AND tenant_id=?",
		boolToInt(online), bridgeID, tenantID)
	return err
}

func (d *DB) TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_seen=datetime('now') WHERE bridge_id=? AND tenant_id=?", bridgeID, tenantID)
	return err
}

func (d *DB) MarkStaleBridgesOffline(ctx context.Context, timeout time.Duration) (int64, error) {
	secs := int(timeout.Seconds())
	res, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET online=0, updated_at=datetime('now') WHERE online=1 AND last_seen IS NOT NULL AND last_seen < datetime('now', ?)",
		fmt.Sprintf("-%d seconds", secs))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE bridges SET last_health=?, updated_at=datetime('now') WHERE bridge_id=? AND tenant_id=?",
		health, bridgeID, tenantID)
	return err
}

func (d *DB) AssociateDeviceWithBridge(ctx context.Context, tenantID string, imei string, bridgeID string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE devices SET bridge_id=? WHERE imei=? AND tenant_id=?", bridgeID, imei, tenantID)
	return err
}

func (d *DB) SetBridgeCredentials(ctx context.Context, tenantID, bridgeID, username, passwordHash string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET mqtt_username=?, mqtt_password_hash=?, updated_at=datetime('now')
		 WHERE bridge_id=? AND tenant_id=?`,
		username, passwordHash, bridgeID, tenantID)
	return err
}

func (d *DB) GetBridgeCredentials(ctx context.Context, tenantID, bridgeID string) (*store.BridgeCredentials, error) {
	var c store.BridgeCredentials
	var certExpiry sql.NullString
	var createdAt string
	err := d.db.QueryRowContext(ctx,
		`SELECT bridge_id, mqtt_username, mqtt_password_hash, cert_pem, cert_expiry, created_at
		 FROM bridges WHERE bridge_id=? AND tenant_id=?`, bridgeID, tenantID,
	).Scan(&c.BridgeID, &c.Username, &c.Password, &c.CertPEM, &certExpiry, &createdAt)
	if err != nil {
		return nil, err
	}
	if certExpiry.Valid {
		t, _ := time.Parse(time.DateTime, certExpiry.String)
		c.CertExpiry = &t
	}
	c.CreatedAt, _ = time.Parse(time.DateTime, createdAt)
	return &c, nil
}

func (d *DB) SetBridgeCertificate(ctx context.Context, tenantID, bridgeID, certPEM string, expiry time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE bridges SET cert_pem=?, cert_expiry=?, updated_at=datetime('now')
		 WHERE bridge_id=? AND tenant_id=?`,
		certPEM, expiry.Format(time.DateTime), bridgeID, tenantID)
	return err
}

func (d *DB) ListBridgesWithCredentials(ctx context.Context) ([]*store.Bridge, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT bridge_id, tenant_id, label, hostname, version, mode,
			location_lat, location_lon, location_alt, capabilities,
			reticulum_hash, reticulum_pubkey, cot_type, cot_callsign,
			online, last_birth, last_health, last_seen,
			mqtt_username, mqtt_password_hash, cert_pem, cert_expiry,
			created_at, updated_at
		 FROM bridges WHERE mqtt_username != '' ORDER BY bridge_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var bridges []*store.Bridge
	for rows.Next() {
		var b store.Bridge
		var online int
		var lastSeen, certExpiry, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.Label, &b.Hostname, &b.Version, &b.Mode,
			&b.LocationLat, &b.LocationLon, &b.LocationAlt, &b.Capabilities,
			&b.ReticulumHash, &b.ReticulumPubkey, &b.CoTType, &b.CoTCallsign,
			&online, &b.LastBirth, &b.LastHealth, &lastSeen,
			&b.MQTTUsername, &b.MQTTPasswordHash, &b.CertPEM, &certExpiry,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		b.Online = online != 0
		if lastSeen.Valid {
			t, _ := time.Parse(time.DateTime, lastSeen.String)
			b.LastSeen = &t
		}
		if certExpiry.Valid {
			t, _ := time.Parse(time.DateTime, certExpiry.String)
			b.CertExpiry = &t
		}
		if createdAt.Valid {
			b.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
		}
		if updatedAt.Valid {
			b.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
		}
		bridges = append(bridges, &b)
	}
	return bridges, nil
}

// joinStrings joins string slices — avoids importing strings package for one call.
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}
