package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Authentication domain of the Postgres store: local users, refresh tokens,
// API keys, per-device encryption keys and WireGuard peers. Semantics follow
// internal/store/mariadb (ID generation, ordering, tenant scoping, not-found =
// sql.ErrNoRows); see helpers.go for the shared conventions.

// --- Users ---

const userColumns = "id, email, name, password_hash, role, enabled, failed_logins, locked_until, last_login_at, created_at, updated_at"

func scanUser(row interface{ Scan(...any) error }) (*store.LocalUser, error) {
	var u store.LocalUser
	var lockedUntil, lastLoginAt sql.NullTime
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.Enabled,
		&u.FailedLogins, &lockedUntil, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	// NULL-able columns: NULL stays the zero time.Time of the struct field.
	if t := utcPtr(lockedUntil); t != nil {
		u.LockedUntil = *t
	}
	if t := utcPtr(lastLoginAt); t != nil {
		u.LastLoginAt = *t
	}
	u.CreatedAt = utc(u.CreatedAt)
	u.UpdatedAt = utc(u.UpdatedAt)
	return &u, nil
}

func (d *DB) CreateUser(ctx context.Context, tenantID string, u *store.LocalUser) error {
	if u.ID == "" {
		u.ID = fmt.Sprintf("usr-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO users (id, email, name, password_hash, role, enabled, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		u.ID, u.Email, u.Name, u.PasswordHash, u.Role, u.Enabled, tenantID)
	return err
}

func (d *DB) GetUserByID(ctx context.Context, tenantID string, id string) (*store.LocalUser, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE id = $1 AND tenant_id = $2", id, tenantID))
}

// GetUserByEmail matches the address case-insensitively; the schema indexes
// (LOWER(email), tenant_id) for exactly this query.
func (d *DB) GetUserByEmail(ctx context.Context, tenantID string, email string) (*store.LocalUser, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE LOWER(email) = LOWER($1) AND tenant_id = $2 LIMIT 1", email, tenantID))
}

func (d *DB) ListUsers(ctx context.Context, tenantID string) ([]store.LocalUser, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, email, name, role, enabled, failed_logins, last_login_at, created_at, updated_at FROM users WHERE tenant_id = $1 ORDER BY created_at", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []store.LocalUser
	for rows.Next() {
		var u store.LocalUser
		var lastLoginAt sql.NullTime
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Enabled,
			&u.FailedLogins, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if lastLoginAt.Valid {
			u.LastLoginAt = lastLoginAt.Time.UTC()
		}
		u.CreatedAt = utc(u.CreatedAt)
		u.UpdatedAt = utc(u.UpdatedAt)
		users = append(users, u)
	}
	return users, rows.Err()
}

func (d *DB) UpdateUser(ctx context.Context, tenantID string, u *store.LocalUser) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE users SET email = $1, name = $2, password_hash = $3, role = $4, enabled = $5, updated_at = now() WHERE id = $6 AND tenant_id = $7",
		u.Email, u.Name, u.PasswordHash, u.Role, u.Enabled, u.ID, tenantID)
	return err
}

func (d *DB) DeleteUser(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM users WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

// IncrementFailedLogins bumps the counter and locks the account for
// store.LockoutDuration once it reaches store.MaxFailedLogins. RETURNING makes
// the update and the read-back one statement; an unknown user yields
// sql.ErrNoRows, where the mariadb store's separate SELECT does the same.
func (d *DB) IncrementFailedLogins(ctx context.Context, tenantID string, id string) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`UPDATE users SET failed_logins = failed_logins + 1,
		 locked_until = CASE WHEN failed_logins + 1 >= $1 THEN now() + $2 * interval '1 second' ELSE locked_until END,
		 updated_at = now()
		 WHERE id = $3 AND tenant_id = $4
		 RETURNING failed_logins`,
		store.MaxFailedLogins, int64(store.LockoutDuration/time.Second), id, tenantID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (d *DB) ResetFailedLogins(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now(), updated_at = now() WHERE id = $1 AND tenant_id = $2",
		id, tenantID)
	return err
}

// --- Refresh Tokens ---

func (d *DB) StoreRefreshToken(ctx context.Context, tenantID string, t *store.RefreshToken) error {
	if t.ID == "" {
		t.ID = fmt.Sprintf("rt-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO refresh_tokens (id, user_id, tenant_id, token_hash, expires_at) VALUES ($1, $2, $3, $4, $5)",
		t.ID, t.UserID, tenantID, t.TokenHash, t.ExpiresAt.UTC())
	return err
}

func (d *DB) GetRefreshToken(ctx context.Context, tokenHash string) (*store.RefreshToken, error) {
	var t store.RefreshToken
	err := d.db.QueryRowContext(ctx,
		"SELECT id, user_id, tenant_id, token_hash, expires_at, created_at FROM refresh_tokens WHERE token_hash = $1", tokenHash,
	).Scan(&t.ID, &t.UserID, &t.TenantID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = utc(t.ExpiresAt)
	t.CreatedAt = utc(t.CreatedAt)
	return &t, nil
}

func (d *DB) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE token_hash = $1", tokenHash)
	return err
}

func (d *DB) DeleteRefreshTokensByUser(ctx context.Context, tenantID string, userID string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE user_id = $1 AND tenant_id = $2", userID, tenantID)
	return err
}

// --- API Keys ---

// last_used and expires_at are NOT NULL with the 1970 sentinel: a zero
// time.Time is written as zeroTime and read back as zero, so callers'
// IsZero checks ("never used", "never expires") behave as on MariaDB.

func (d *DB) CreateAPIKey(ctx context.Context, tenantID string, k *store.APIKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("key-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO api_keys (id, key_hash, key_prefix, role, label, device_imei, expires_at, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		k.ID, k.KeyHash, k.KeyPrefix, k.Role, k.Label, k.DeviceIMEI, sentinelTime(k.ExpiresAt), tenantID)
	return err
}

func (d *DB) GetAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKey, string, error) {
	var k store.APIKey
	var tenantID string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, created_at, tenant_id FROM api_keys WHERE key_hash = $1", keyHash,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt, &tenantID)
	if err != nil {
		return nil, "", err
	}
	normaliseAPIKey(&k)
	return &k, tenantID, nil
}

func (d *DB) ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, created_at FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		normaliseAPIKey(&k)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) GetAPIKeyByID(ctx context.Context, tenantID string, id string) (*store.APIKey, error) {
	var k store.APIKey
	err := d.db.QueryRowContext(ctx,
		"SELECT id, key_hash, key_prefix, role, label, device_imei, last_used, expires_at, rotation_days, created_at FROM api_keys WHERE id = $1 AND tenant_id = $2",
		id, tenantID,
	).Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.RotationDays, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	normaliseAPIKey(&k)
	return &k, nil
}

// ListExpiringAPIKeys returns keys (all tenants) whose expiry is set and lies
// at or before `before`, soonest first. The sentinel means "never expires"
// and is excluded, as MariaDB's `expires_at != '0001-01-01'` did.
func (d *DB) ListExpiringAPIKeys(ctx context.Context, before time.Time, limit int) ([]store.APIKey, error) {
	query := `SELECT id, key_prefix, role, label, device_imei, last_used, expires_at, rotation_days, created_at
		FROM api_keys WHERE expires_at > $1 AND expires_at <= $2
		ORDER BY expires_at ASC`
	args := []any{zeroTime, before.UTC()}
	if limit > 0 {
		query += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.APIKey
	for rows.Next() {
		var k store.APIKey
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Role, &k.Label, &k.DeviceIMEI, &k.LastUsed, &k.ExpiresAt, &k.RotationDays, &k.CreatedAt); err != nil {
			return nil, err
		}
		normaliseAPIKey(&k)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) UpdateAPIKeySecret(ctx context.Context, tenantID string, id string, keyHash, keyPrefix string, expiresAt time.Time) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE api_keys SET key_hash = $1, key_prefix = $2, expires_at = $3 WHERE id = $4 AND tenant_id = $5",
		keyHash, keyPrefix, sentinelTime(expiresAt), id, tenantID)
	return err
}

func (d *DB) DeleteAPIKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

func (d *DB) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE api_keys SET last_used = now() WHERE id = $1", id)
	return err
}

// normaliseAPIKey maps sentinel timestamps back to zero and the rest to UTC.
func normaliseAPIKey(k *store.APIKey) {
	k.LastUsed = fromSentinel(k.LastUsed)
	k.ExpiresAt = fromSentinel(k.ExpiresAt)
	k.CreatedAt = utc(k.CreatedAt)
}

// --- Device Encryption Keys ---

func (d *DB) CreateDeviceKey(ctx context.Context, tenantID string, k *store.DeviceKey) error {
	if k.ID == "" {
		k.ID = fmt.Sprintf("dk-%d", time.Now().UnixNano())
	}
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_keys (id, device_imei, key_hash, key_hex, mode, tenant_id) VALUES ($1, $2, $3, $4, $5, $6)",
		k.ID, k.DeviceIMEI, k.KeyHash, k.KeyHex, k.Mode, tenantID)
	if err != nil {
		return err
	}
	k.CreatedAt = time.Now().UTC()
	return nil
}

// ListDeviceKeys omits key_hex (listings never expose key material).
func (d *DB) ListDeviceKeys(ctx context.Context, tenantID string, deviceIMEI string) ([]store.DeviceKey, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, device_imei, key_hash, mode, created_at FROM device_keys WHERE device_imei = $1 AND tenant_id = $2 ORDER BY created_at DESC",
		deviceIMEI, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []store.DeviceKey
	for rows.Next() {
		var k store.DeviceKey
		if err := rows.Scan(&k.ID, &k.DeviceIMEI, &k.KeyHash, &k.Mode, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.CreatedAt = utc(k.CreatedAt)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) GetDeviceKeyLatest(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceKey, error) {
	var k store.DeviceKey
	err := d.db.QueryRowContext(ctx,
		"SELECT id, device_imei, key_hash, key_hex, mode, created_at FROM device_keys WHERE device_imei = $1 AND tenant_id = $2 ORDER BY created_at DESC LIMIT 1",
		deviceIMEI, tenantID).Scan(&k.ID, &k.DeviceIMEI, &k.KeyHash, &k.KeyHex, &k.Mode, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.CreatedAt = utc(k.CreatedAt)
	return &k, nil
}

func (d *DB) DeleteDeviceKey(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM device_keys WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

// --- Device WireGuard ---

// SaveDeviceWireguard upserts the peer row. MariaDB used REPLACE INTO, which
// re-inserts the row and so resets created_at; the DO UPDATE branch sets
// created_at = now() to keep that behaviour.
func (d *DB) SaveDeviceWireguard(ctx context.Context, tenantID string, dw *store.DeviceWireguard) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO device_wireguard (device_imei, peer_id, vpn_address, public_key, tenant_id)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (device_imei, tenant_id) DO UPDATE SET
			peer_id = EXCLUDED.peer_id, vpn_address = EXCLUDED.vpn_address,
			public_key = EXCLUDED.public_key, created_at = now()`,
		dw.DeviceIMEI, dw.PeerID, dw.VPNAddress, dw.PublicKey, tenantID)
	if err != nil {
		return err
	}
	dw.CreatedAt = time.Now().UTC()
	return nil
}

func (d *DB) GetDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) (*store.DeviceWireguard, error) {
	var dw store.DeviceWireguard
	err := d.db.QueryRowContext(ctx,
		"SELECT device_imei, peer_id, vpn_address, public_key, created_at FROM device_wireguard WHERE device_imei = $1 AND tenant_id = $2",
		deviceIMEI, tenantID).Scan(&dw.DeviceIMEI, &dw.PeerID, &dw.VPNAddress, &dw.PublicKey, &dw.CreatedAt)
	if err != nil {
		return nil, err
	}
	dw.CreatedAt = utc(dw.CreatedAt)
	return &dw, nil
}

func (d *DB) DeleteDeviceWireguard(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM device_wireguard WHERE device_imei = $1 AND tenant_id = $2", deviceIMEI, tenantID)
	return err
}
