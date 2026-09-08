package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Tests for auth.go and routing_alerts.go against a real Postgres (testDB).

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func wantNoRows(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("%s: want sql.ErrNoRows, got %v", what, err)
	}
}

// --- Users ---

func TestUsersCRUDAndTenantIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	u := &store.LocalUser{Email: "Alice@Example.com", Name: "Alice", PasswordHash: "h1", Role: "owner", Enabled: true}
	mustNoErr(t, db.CreateUser(ctx, "t1", u), "CreateUser")
	if u.ID == "" || u.ID[:4] != "usr-" {
		t.Fatalf("expected generated usr- ID, got %q", u.ID)
	}
	explicit := &store.LocalUser{ID: "usr-fixed", Email: "bob@example.com", PasswordHash: "h2", Role: "viewer", Enabled: false}
	mustNoErr(t, db.CreateUser(ctx, "t1", explicit), "CreateUser explicit")
	if explicit.ID != "usr-fixed" {
		t.Fatalf("explicit ID overwritten: %q", explicit.ID)
	}
	// Same email in another tenant is allowed and invisible across tenants.
	other := &store.LocalUser{Email: "alice@example.com", PasswordHash: "h3", Role: "viewer", Enabled: true}
	mustNoErr(t, db.CreateUser(ctx, "t2", other), "CreateUser t2")

	got, err := db.GetUserByID(ctx, "t1", u.ID)
	mustNoErr(t, err, "GetUserByID")
	if got.Email != "Alice@Example.com" || got.PasswordHash != "h1" || got.Role != "owner" || !got.Enabled {
		t.Fatalf("GetUserByID mismatch: %+v", got)
	}
	if !got.LockedUntil.IsZero() || !got.LastLoginAt.IsZero() || got.FailedLogins != 0 {
		t.Fatalf("fresh user must have zero lock/login fields: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at not UTC/populated: %v", got.CreatedAt)
	}
	_, err = db.GetUserByID(ctx, "t2", u.ID)
	wantNoRows(t, err, "GetUserByID cross-tenant")
	_, err = db.GetUserByID(ctx, "t1", "nope")
	wantNoRows(t, err, "GetUserByID unknown")

	// Case-insensitive email lookup, tenant-scoped.
	for _, e := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Alice@Example.com"} {
		byEmail, err := db.GetUserByEmail(ctx, "t1", e)
		mustNoErr(t, err, "GetUserByEmail "+e)
		if byEmail.ID != u.ID {
			t.Fatalf("GetUserByEmail(%q) = %s, want %s", e, byEmail.ID, u.ID)
		}
	}
	byEmail, err := db.GetUserByEmail(ctx, "t2", "ALICE@example.com")
	mustNoErr(t, err, "GetUserByEmail t2")
	if byEmail.ID != other.ID {
		t.Fatalf("t2 lookup returned %s, want %s", byEmail.ID, other.ID)
	}
	_, err = db.GetUserByEmail(ctx, "t1", "nobody@example.com")
	wantNoRows(t, err, "GetUserByEmail unknown")

	list, err := db.ListUsers(ctx, "t1")
	mustNoErr(t, err, "ListUsers")
	if len(list) != 2 || list[0].ID != u.ID || list[1].ID != "usr-fixed" {
		t.Fatalf("ListUsers t1 = %+v", list)
	}
	if list[0].PasswordHash != "" {
		t.Fatal("ListUsers must not return password hashes")
	}
	list2, err := db.ListUsers(ctx, "t2")
	mustNoErr(t, err, "ListUsers t2")
	if len(list2) != 1 {
		t.Fatalf("ListUsers t2 = %d rows", len(list2))
	}

	u.Name = "Alice B"
	u.Role = "operator"
	u.Enabled = false
	u.PasswordHash = "h1b"
	mustNoErr(t, db.UpdateUser(ctx, "t1", u), "UpdateUser")
	got, err = db.GetUserByID(ctx, "t1", u.ID)
	mustNoErr(t, err, "GetUserByID after update")
	if got.Name != "Alice B" || got.Role != "operator" || got.Enabled || got.PasswordHash != "h1b" {
		t.Fatalf("UpdateUser not applied: %+v", got)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("updated_at %v before created_at %v", got.UpdatedAt, got.CreatedAt)
	}
	// Cross-tenant update must be a no-op.
	mustNoErr(t, db.UpdateUser(ctx, "t2", &store.LocalUser{ID: u.ID, Email: "x@x", Role: "owner"}), "UpdateUser cross-tenant")
	got, _ = db.GetUserByID(ctx, "t1", u.ID)
	if got.Email != "Alice@Example.com" {
		t.Fatalf("cross-tenant update leaked: %+v", got)
	}

	mustNoErr(t, db.DeleteUser(ctx, "t2", u.ID), "DeleteUser cross-tenant")
	if _, err := db.GetUserByID(ctx, "t1", u.ID); err != nil {
		t.Fatalf("cross-tenant delete removed user: %v", err)
	}
	mustNoErr(t, db.DeleteUser(ctx, "t1", u.ID), "DeleteUser")
	_, err = db.GetUserByID(ctx, "t1", u.ID)
	wantNoRows(t, err, "GetUserByID after delete")
}

func TestFailedLoginsIncrementAndReset(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	u := &store.LocalUser{Email: "lock@example.com", PasswordHash: "h", Role: "viewer", Enabled: true}
	mustNoErr(t, db.CreateUser(ctx, "t1", u), "CreateUser")

	for i := 1; i < store.MaxFailedLogins; i++ {
		n, err := db.IncrementFailedLogins(ctx, "t1", u.ID)
		mustNoErr(t, err, "IncrementFailedLogins")
		if n != i {
			t.Fatalf("increment %d returned %d", i, n)
		}
	}
	got, err := db.GetUserByID(ctx, "t1", u.ID)
	mustNoErr(t, err, "GetUserByID")
	if !got.LockedUntil.IsZero() {
		t.Fatalf("locked before threshold: %v", got.LockedUntil)
	}

	n, err := db.IncrementFailedLogins(ctx, "t1", u.ID)
	mustNoErr(t, err, "IncrementFailedLogins threshold")
	if n != store.MaxFailedLogins {
		t.Fatalf("threshold increment returned %d", n)
	}
	got, err = db.GetUserByID(ctx, "t1", u.ID)
	mustNoErr(t, err, "GetUserByID locked")
	if got.LockedUntil.IsZero() || got.FailedLogins != store.MaxFailedLogins {
		t.Fatalf("expected lockout: %+v", got)
	}
	remaining := time.Until(got.LockedUntil)
	if remaining < store.LockoutDuration-time.Minute || remaining > store.LockoutDuration+time.Minute {
		t.Fatalf("lockout window %v, want ~%v", remaining, store.LockoutDuration)
	}
	// Same email in another tenant is untouched; unknown user is not found.
	if _, err := db.IncrementFailedLogins(ctx, "t2", u.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant increment: want ErrNoRows, got %v", err)
	}

	mustNoErr(t, db.ResetFailedLogins(ctx, "t1", u.ID), "ResetFailedLogins")
	got, err = db.GetUserByID(ctx, "t1", u.ID)
	mustNoErr(t, err, "GetUserByID reset")
	if got.FailedLogins != 0 || !got.LockedUntil.IsZero() || got.LastLoginAt.IsZero() {
		t.Fatalf("reset not applied: %+v", got)
	}
	if time.Since(got.LastLoginAt) > time.Minute {
		t.Fatalf("last_login_at not recent: %v", got.LastLoginAt)
	}
	list, _ := db.ListUsers(ctx, "t1")
	if len(list) != 1 || list[0].LastLoginAt.IsZero() {
		t.Fatalf("ListUsers missing last_login_at: %+v", list)
	}
}

// --- Refresh tokens ---

func TestRefreshTokens(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	exp := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
	tok := &store.RefreshToken{UserID: "usr-1", TokenHash: "hash-a", ExpiresAt: exp}
	mustNoErr(t, db.StoreRefreshToken(ctx, "t1", tok), "StoreRefreshToken")
	if tok.ID == "" || tok.ID[:3] != "rt-" {
		t.Fatalf("expected rt- ID, got %q", tok.ID)
	}
	mustNoErr(t, db.StoreRefreshToken(ctx, "t1", &store.RefreshToken{ID: "rt-b", UserID: "usr-1", TokenHash: "hash-b", ExpiresAt: exp}), "StoreRefreshToken b")
	mustNoErr(t, db.StoreRefreshToken(ctx, "t2", &store.RefreshToken{UserID: "usr-1", TokenHash: "hash-c", ExpiresAt: exp}), "StoreRefreshToken c")
	// token_hash is UNIQUE.
	if err := db.StoreRefreshToken(ctx, "t1", &store.RefreshToken{UserID: "usr-9", TokenHash: "hash-a", ExpiresAt: exp}); err == nil {
		t.Fatal("duplicate token_hash must fail")
	}

	got, err := db.GetRefreshToken(ctx, "hash-a")
	mustNoErr(t, err, "GetRefreshToken")
	if got.ID != tok.ID || got.UserID != "usr-1" || got.TenantID != "t1" || got.TokenHash != "hash-a" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("GetRefreshToken mismatch: %+v", got)
	}
	if got.ExpiresAt.Location() != time.UTC || got.CreatedAt.IsZero() {
		t.Fatalf("timestamps: %+v", got)
	}
	_, err = db.GetRefreshToken(ctx, "missing")
	wantNoRows(t, err, "GetRefreshToken missing")

	mustNoErr(t, db.DeleteRefreshToken(ctx, "hash-a"), "DeleteRefreshToken")
	_, err = db.GetRefreshToken(ctx, "hash-a")
	wantNoRows(t, err, "GetRefreshToken after delete")

	// Delete by user is tenant-scoped: t2's token for the same user survives.
	mustNoErr(t, db.DeleteRefreshTokensByUser(ctx, "t1", "usr-1"), "DeleteRefreshTokensByUser")
	_, err = db.GetRefreshToken(ctx, "hash-b")
	wantNoRows(t, err, "hash-b after user delete")
	if _, err := db.GetRefreshToken(ctx, "hash-c"); err != nil {
		t.Fatalf("t2 token deleted by t1 scope: %v", err)
	}
}

// --- API keys ---

func TestAPIKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	k := &store.APIKey{KeyHash: "kh-1", KeyPrefix: "meshsat_ab12", Role: "operator", Label: "ci", DeviceIMEI: "3001"}
	mustNoErr(t, db.CreateAPIKey(ctx, "t1", k), "CreateAPIKey")
	if k.ID == "" || k.ID[:4] != "key-" {
		t.Fatalf("expected key- ID, got %q", k.ID)
	}
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	k2 := &store.APIKey{ID: "key-2", KeyHash: "kh-2", KeyPrefix: "meshsat_cd34", Role: "viewer", Label: "expiring", ExpiresAt: exp}
	mustNoErr(t, db.CreateAPIKey(ctx, "t1", k2), "CreateAPIKey k2")
	k3 := &store.APIKey{ID: "key-3", KeyHash: "kh-3", KeyPrefix: "meshsat_ef56", Role: "owner", Label: "other tenant", ExpiresAt: exp.Add(-time.Hour)}
	mustNoErr(t, db.CreateAPIKey(ctx, "t2", k3), "CreateAPIKey k3")
	if err := db.CreateAPIKey(ctx, "t2", &store.APIKey{KeyHash: "kh-1"}); err == nil {
		t.Fatal("duplicate key_hash must fail")
	}

	// Lookup by hash is global and returns the owning tenant.
	got, tenant, err := db.GetAPIKeyByHash(ctx, "kh-1")
	mustNoErr(t, err, "GetAPIKeyByHash")
	if tenant != "t1" || got.ID != k.ID || got.KeyHash != "kh-1" || got.Role != "operator" || got.DeviceIMEI != "3001" {
		t.Fatalf("GetAPIKeyByHash mismatch: %+v tenant=%s", got, tenant)
	}
	if !got.LastUsed.IsZero() || !got.ExpiresAt.IsZero() {
		t.Fatalf("sentinels must read back as zero: last_used=%v expires=%v", got.LastUsed, got.ExpiresAt)
	}
	if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at: %v", got.CreatedAt)
	}
	_, _, err = db.GetAPIKeyByHash(ctx, "kh-x")
	wantNoRows(t, err, "GetAPIKeyByHash missing")

	// Lookup by ID is tenant-scoped and includes rotation_days.
	byID, err := db.GetAPIKeyByID(ctx, "t1", "key-2")
	mustNoErr(t, err, "GetAPIKeyByID")
	if !byID.ExpiresAt.Equal(exp) || byID.RotationDays != 0 || byID.KeyHash != "kh-2" {
		t.Fatalf("GetAPIKeyByID mismatch: %+v", byID)
	}
	_, err = db.GetAPIKeyByID(ctx, "t2", "key-2")
	wantNoRows(t, err, "GetAPIKeyByID cross-tenant")

	list, err := db.ListAPIKeys(ctx, "t1")
	mustNoErr(t, err, "ListAPIKeys")
	if len(list) != 2 || list[0].ID != "key-2" || list[1].ID != k.ID {
		t.Fatalf("ListAPIKeys t1 (newest first) = %+v", list)
	}
	if list[0].KeyHash != "" {
		t.Fatal("ListAPIKeys must not return key hashes")
	}
	list2, _ := db.ListAPIKeys(ctx, "t2")
	if len(list2) != 1 || list2[0].ID != "key-3" {
		t.Fatalf("ListAPIKeys t2 = %+v", list2)
	}

	// Expiring: all tenants, soonest first, never-expiring (sentinel) excluded.
	expiring, err := db.ListExpiringAPIKeys(ctx, exp.Add(time.Hour), 0)
	mustNoErr(t, err, "ListExpiringAPIKeys")
	if len(expiring) != 2 || expiring[0].ID != "key-3" || expiring[1].ID != "key-2" {
		t.Fatalf("ListExpiringAPIKeys = %+v", expiring)
	}
	expiring, err = db.ListExpiringAPIKeys(ctx, exp.Add(time.Hour), 1)
	mustNoErr(t, err, "ListExpiringAPIKeys limit")
	if len(expiring) != 1 || expiring[0].ID != "key-3" {
		t.Fatalf("ListExpiringAPIKeys limit=1 = %+v", expiring)
	}
	expiring, err = db.ListExpiringAPIKeys(ctx, time.Now(), 0)
	mustNoErr(t, err, "ListExpiringAPIKeys none")
	if len(expiring) != 0 {
		t.Fatalf("no key should expire before now: %+v", expiring)
	}

	// Touch last_used (global by ID), then it is no longer zero.
	mustNoErr(t, db.TouchAPIKeyLastUsed(ctx, k.ID), "TouchAPIKeyLastUsed")
	got, _, err = db.GetAPIKeyByHash(ctx, "kh-1")
	mustNoErr(t, err, "GetAPIKeyByHash after touch")
	if got.LastUsed.IsZero() || time.Since(got.LastUsed) > time.Minute {
		t.Fatalf("last_used not touched: %v", got.LastUsed)
	}

	// Rotate the secret; clearing the expiry writes the sentinel again.
	mustNoErr(t, db.UpdateAPIKeySecret(ctx, "t1", "key-2", "kh-2b", "meshsat_zz99", time.Time{}), "UpdateAPIKeySecret")
	_, _, err = db.GetAPIKeyByHash(ctx, "kh-2")
	wantNoRows(t, err, "old hash after rotation")
	rot, tenant, err := db.GetAPIKeyByHash(ctx, "kh-2b")
	mustNoErr(t, err, "new hash after rotation")
	if tenant != "t1" || rot.KeyPrefix != "meshsat_zz99" || !rot.ExpiresAt.IsZero() {
		t.Fatalf("rotation mismatch: %+v", rot)
	}
	mustNoErr(t, db.UpdateAPIKeySecret(ctx, "t2", "key-2", "kh-hijack", "x", exp), "UpdateAPIKeySecret cross-tenant")
	if _, _, err := db.GetAPIKeyByHash(ctx, "kh-hijack"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-tenant secret rotation must be a no-op")
	}

	mustNoErr(t, db.DeleteAPIKey(ctx, "t2", k.ID), "DeleteAPIKey cross-tenant")
	if _, _, err := db.GetAPIKeyByHash(ctx, "kh-1"); err != nil {
		t.Fatalf("cross-tenant delete removed key: %v", err)
	}
	mustNoErr(t, db.DeleteAPIKey(ctx, "t1", k.ID), "DeleteAPIKey")
	_, _, err = db.GetAPIKeyByHash(ctx, "kh-1")
	wantNoRows(t, err, "GetAPIKeyByHash after delete")
}

// --- Device keys ---

func TestDeviceKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	k1 := &store.DeviceKey{DeviceIMEI: "3001", KeyHash: "dkh-1", KeyHex: "aa", Mode: "decrypt"}
	mustNoErr(t, db.CreateDeviceKey(ctx, "t1", k1), "CreateDeviceKey")
	if k1.ID == "" || k1.ID[:3] != "dk-" || k1.CreatedAt.IsZero() {
		t.Fatalf("CreateDeviceKey did not populate: %+v", k1)
	}
	time.Sleep(2 * time.Millisecond) // distinct created_at for ordering
	k2 := &store.DeviceKey{ID: "dk-2", DeviceIMEI: "3001", KeyHash: "dkh-2", KeyHex: "", Mode: "passthrough"}
	mustNoErr(t, db.CreateDeviceKey(ctx, "t1", k2), "CreateDeviceKey k2")
	mustNoErr(t, db.CreateDeviceKey(ctx, "t2", &store.DeviceKey{DeviceIMEI: "3001", KeyHash: "dkh-3", Mode: "decrypt"}), "CreateDeviceKey t2")

	list, err := db.ListDeviceKeys(ctx, "t1", "3001")
	mustNoErr(t, err, "ListDeviceKeys")
	if len(list) != 2 || list[0].ID != "dk-2" || list[1].ID != k1.ID {
		t.Fatalf("ListDeviceKeys (newest first) = %+v", list)
	}
	if list[1].KeyHex != "" {
		t.Fatal("ListDeviceKeys must not return key material")
	}
	list, err = db.ListDeviceKeys(ctx, "t1", "9999")
	mustNoErr(t, err, "ListDeviceKeys none")
	if len(list) != 0 {
		t.Fatalf("expected no keys, got %+v", list)
	}

	latest, err := db.GetDeviceKeyLatest(ctx, "t1", "3001")
	mustNoErr(t, err, "GetDeviceKeyLatest")
	if latest.ID != "dk-2" || latest.Mode != "passthrough" {
		t.Fatalf("GetDeviceKeyLatest = %+v", latest)
	}
	mustNoErr(t, db.DeleteDeviceKey(ctx, "t1", "dk-2"), "DeleteDeviceKey")
	latest, err = db.GetDeviceKeyLatest(ctx, "t1", "3001")
	mustNoErr(t, err, "GetDeviceKeyLatest after delete")
	if latest.ID != k1.ID || latest.KeyHex != "aa" {
		t.Fatalf("GetDeviceKeyLatest must include key_hex: %+v", latest)
	}
	mustNoErr(t, db.DeleteDeviceKey(ctx, "t2", k1.ID), "DeleteDeviceKey cross-tenant")
	if _, err := db.GetDeviceKeyLatest(ctx, "t1", "3001"); err != nil {
		t.Fatalf("cross-tenant delete removed key: %v", err)
	}
	_, err = db.GetDeviceKeyLatest(ctx, "t3", "3001")
	wantNoRows(t, err, "GetDeviceKeyLatest unknown tenant")
}

// --- Device WireGuard ---

func TestDeviceWireguard(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	dw := &store.DeviceWireguard{DeviceIMEI: "3001", PeerID: "p1", VPNAddress: "10.8.0.5/32", PublicKey: "pk1"}
	mustNoErr(t, db.SaveDeviceWireguard(ctx, "t1", dw), "SaveDeviceWireguard")
	if dw.CreatedAt.IsZero() {
		t.Fatal("SaveDeviceWireguard must set CreatedAt")
	}
	got, err := db.GetDeviceWireguard(ctx, "t1", "3001")
	mustNoErr(t, err, "GetDeviceWireguard")
	if got.PeerID != "p1" || got.VPNAddress != "10.8.0.5/32" || got.PublicKey != "pk1" || got.CreatedAt.IsZero() {
		t.Fatalf("GetDeviceWireguard = %+v", got)
	}
	// Upsert on (device_imei, tenant_id).
	mustNoErr(t, db.SaveDeviceWireguard(ctx, "t1", &store.DeviceWireguard{DeviceIMEI: "3001", PeerID: "p2", VPNAddress: "10.8.0.6/32", PublicKey: "pk2"}), "SaveDeviceWireguard upsert")
	got, err = db.GetDeviceWireguard(ctx, "t1", "3001")
	mustNoErr(t, err, "GetDeviceWireguard after upsert")
	if got.PeerID != "p2" || got.VPNAddress != "10.8.0.6/32" || got.PublicKey != "pk2" {
		t.Fatalf("upsert not applied: %+v", got)
	}
	_, err = db.GetDeviceWireguard(ctx, "t2", "3001")
	wantNoRows(t, err, "GetDeviceWireguard cross-tenant")

	mustNoErr(t, db.DeleteDeviceWireguard(ctx, "t2", "3001"), "DeleteDeviceWireguard cross-tenant")
	if _, err := db.GetDeviceWireguard(ctx, "t1", "3001"); err != nil {
		t.Fatalf("cross-tenant delete removed peer: %v", err)
	}
	mustNoErr(t, db.DeleteDeviceWireguard(ctx, "t1", "3001"), "DeleteDeviceWireguard")
	_, err = db.GetDeviceWireguard(ctx, "t1", "3001")
	wantNoRows(t, err, "GetDeviceWireguard after delete")
}

// --- Routes ---

func TestRoutes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	r := &store.Route{Name: "zeta", SourceType: "iridium", DestinationType: "sms", Filter: "sos", Enabled: true}
	mustNoErr(t, db.CreateRoute(ctx, "t1", r), "CreateRoute")
	if r.ID == "" || r.ID[:6] != "route-" || r.CreatedAt.IsZero() || !r.UpdatedAt.Equal(r.CreatedAt) {
		t.Fatalf("CreateRoute did not populate: %+v", r)
	}
	mustNoErr(t, db.CreateRoute(ctx, "t1", &store.Route{ID: "route-a", Name: "alpha", SourceType: "mqtt", DestinationType: "webhook"}), "CreateRoute a")
	mustNoErr(t, db.CreateRoute(ctx, "t2", &store.Route{Name: "beta", SourceType: "mqtt", DestinationType: "tak", Enabled: true}), "CreateRoute t2")

	got, err := db.GetRoute(ctx, "t1", r.ID)
	mustNoErr(t, err, "GetRoute")
	if got.Name != "zeta" || got.Filter != "sos" || !got.Enabled || got.DestinationType != "sms" {
		t.Fatalf("GetRoute = %+v", got)
	}
	if !got.CreatedAt.Equal(r.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("created_at round trip: %v vs %v", got.CreatedAt, r.CreatedAt)
	}
	_, err = db.GetRoute(ctx, "t2", r.ID)
	wantNoRows(t, err, "GetRoute cross-tenant")

	list, err := db.ListRoutes(ctx, "t1")
	mustNoErr(t, err, "ListRoutes")
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Fatalf("ListRoutes (by name) = %+v", list)
	}

	r.Name = "zeta2"
	r.Enabled = false
	r.Filter = ""
	before := r.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	mustNoErr(t, db.UpdateRoute(ctx, "t1", r), "UpdateRoute")
	if !r.UpdatedAt.After(before) {
		t.Fatal("UpdateRoute must bump UpdatedAt")
	}
	got, err = db.GetRoute(ctx, "t1", r.ID)
	mustNoErr(t, err, "GetRoute after update")
	if got.Name != "zeta2" || got.Enabled || got.Filter != "" || !got.UpdatedAt.Equal(r.UpdatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("UpdateRoute not applied: %+v", got)
	}
	mustNoErr(t, db.UpdateRoute(ctx, "t2", &store.Route{ID: r.ID, Name: "hijack"}), "UpdateRoute cross-tenant")
	got, _ = db.GetRoute(ctx, "t1", r.ID)
	if got.Name != "zeta2" {
		t.Fatal("cross-tenant update leaked")
	}

	mustNoErr(t, db.DeleteRoute(ctx, "t2", r.ID), "DeleteRoute cross-tenant")
	if _, err := db.GetRoute(ctx, "t1", r.ID); err != nil {
		t.Fatalf("cross-tenant delete removed route: %v", err)
	}
	mustNoErr(t, db.DeleteRoute(ctx, "t1", r.ID), "DeleteRoute")
	_, err = db.GetRoute(ctx, "t1", r.ID)
	wantNoRows(t, err, "GetRoute after delete")
}

// --- Escalation chains ---

func TestEscalationChains(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	tiers := []store.EscalationTier{
		{Name: "sms_oncall", Targets: []string{"+31600000000"}, WaitSec: 300, MaxRetries: 2},
		{Name: "email_team", Targets: []string{"mailto://ops@example.com"}, WaitSec: 600, MaxRetries: 1},
	}
	c := &store.EscalationChain{Name: "default", Tiers: tiers}
	mustNoErr(t, db.CreateEscalationChain(ctx, "t1", c), "CreateEscalationChain")
	if c.ID == "" || c.ID[:6] != "chain-" {
		t.Fatalf("expected chain- ID, got %q", c.ID)
	}
	time.Sleep(2 * time.Millisecond)
	mustNoErr(t, db.CreateEscalationChain(ctx, "t1", &store.EscalationChain{ID: "chain-empty", Name: "empty"}), "CreateEscalationChain empty tiers")
	mustNoErr(t, db.CreateEscalationChain(ctx, "t2", &store.EscalationChain{ID: "chain-t2", Name: "t2"}), "CreateEscalationChain t2")

	got, err := db.GetEscalationChain(ctx, "t1", c.ID)
	mustNoErr(t, err, "GetEscalationChain")
	if got.Name != "default" || len(got.Tiers) != 2 || got.Tiers[1].Name != "email_team" || got.Tiers[0].Targets[0] != "+31600000000" || got.Tiers[0].WaitSec != 300 {
		t.Fatalf("GetEscalationChain tiers mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps: %+v", got)
	}
	// Empty tenant = any tenant (engine wildcard); wrong tenant = not found.
	if _, err := db.GetEscalationChain(ctx, "", "chain-t2"); err != nil {
		t.Fatalf("GetEscalationChain wildcard tenant: %v", err)
	}
	_, err = db.GetEscalationChain(ctx, "t1", "chain-t2")
	wantNoRows(t, err, "GetEscalationChain cross-tenant")
	empty, err := db.GetEscalationChain(ctx, "t1", "chain-empty")
	mustNoErr(t, err, "GetEscalationChain empty")
	if len(empty.Tiers) != 0 {
		t.Fatalf("empty chain tiers = %+v", empty.Tiers)
	}

	list, err := db.ListEscalationChains(ctx, "t1")
	mustNoErr(t, err, "ListEscalationChains")
	if len(list) != 2 || list[0].ID != "chain-empty" || list[1].ID != c.ID {
		t.Fatalf("ListEscalationChains (newest first) = %+v", list)
	}
	if len(list[1].Tiers) != 2 {
		t.Fatalf("ListEscalationChains lost tiers: %+v", list[1])
	}

	mustNoErr(t, db.DeleteEscalationChain(ctx, "t2", c.ID), "DeleteEscalationChain cross-tenant")
	if _, err := db.GetEscalationChain(ctx, "t1", c.ID); err != nil {
		t.Fatalf("cross-tenant delete removed chain: %v", err)
	}
	mustNoErr(t, db.DeleteEscalationChain(ctx, "t1", c.ID), "DeleteEscalationChain")
	_, err = db.GetEscalationChain(ctx, "t1", c.ID)
	wantNoRows(t, err, "GetEscalationChain after delete")
}

// --- Alerts ---

func TestAlerts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	mk := func(id, tenant, state string, offset time.Duration) *store.Alert {
		return &store.Alert{
			ID: id, TenantID: tenant, ChainID: "chain-1", DeviceIMEI: "3001", Type: "sos",
			Detail: "help", State: state, CurrentTier: 0, Retries: 0,
			NextEscAt: base.Add(offset + 5*time.Minute), CreatedAt: base.Add(offset), UpdatedAt: base.Add(offset),
		}
	}
	mustNoErr(t, db.CreateAlert(ctx, "t1", mk("al-1", "t1", store.AlertStateTriggered, 0)), "CreateAlert 1")
	mustNoErr(t, db.CreateAlert(ctx, "t1", mk("al-2", "t1", store.AlertStateAcknowledged, time.Minute)), "CreateAlert 2")
	mustNoErr(t, db.CreateAlert(ctx, "t2", mk("al-3", "t2", store.AlertStateEscalating, 2*time.Minute)), "CreateAlert 3")
	mustNoErr(t, db.CreateAlert(ctx, "t2", mk("al-4", "t2", store.AlertStateExhausted, 3*time.Minute)), "CreateAlert 4")
	mustNoErr(t, db.CreateAlert(ctx, "t3", mk("al-5", "t3", store.AlertStateTriggered, 4*time.Minute)), "CreateAlert 5")

	got, err := db.GetAlert(ctx, "t1", "al-1")
	mustNoErr(t, err, "GetAlert")
	if got.TenantID != "t1" || got.ChainID != "chain-1" || got.Type != "sos" || got.Detail != "help" || got.State != store.AlertStateTriggered {
		t.Fatalf("GetAlert = %+v", got)
	}
	if !got.AckedAt.IsZero() || got.AckedBy != "" {
		t.Fatalf("unacked alert must read zero acked_at: %+v", got)
	}
	if !got.NextEscAt.Equal(base.Add(5*time.Minute)) || !got.CreatedAt.Equal(base) || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps: %+v", got)
	}
	// Empty tenant = any tenant; wrong tenant = not found.
	if _, err := db.GetAlert(ctx, "", "al-3"); err != nil {
		t.Fatalf("GetAlert wildcard tenant: %v", err)
	}
	_, err = db.GetAlert(ctx, "t1", "al-3")
	wantNoRows(t, err, "GetAlert cross-tenant")
	_, err = db.GetAlert(ctx, "t1", "nope")
	wantNoRows(t, err, "GetAlert unknown")

	// Engine case: all tenants, active only, newest first.
	active, err := db.ListAlerts(ctx, "", true, 100)
	mustNoErr(t, err, "ListAlerts all tenants active")
	if len(active) != 3 || active[0].ID != "al-5" || active[1].ID != "al-3" || active[2].ID != "al-1" {
		t.Fatalf("ListAlerts(\"\", active) = %+v", active)
	}
	if active[0].TenantID != "t3" || active[1].TenantID != "t2" || active[2].TenantID != "t1" {
		t.Fatal("ListAlerts must carry each alert's TenantID for the engine")
	}
	// All tenants, all states, limit applied.
	all, err := db.ListAlerts(ctx, "", false, 2)
	mustNoErr(t, err, "ListAlerts limit")
	if len(all) != 2 || all[0].ID != "al-5" || all[1].ID != "al-4" {
		t.Fatalf("ListAlerts(\"\", all, 2) = %+v", all)
	}
	// Tenant-scoped.
	t1, err := db.ListAlerts(ctx, "t1", false, 50)
	mustNoErr(t, err, "ListAlerts t1")
	if len(t1) != 2 || t1[0].ID != "al-2" || t1[1].ID != "al-1" {
		t.Fatalf("ListAlerts(t1) = %+v", t1)
	}
	t1active, err := db.ListAlerts(ctx, "t1", true, 50)
	mustNoErr(t, err, "ListAlerts t1 active")
	if len(t1active) != 1 || t1active[0].ID != "al-1" {
		t.Fatalf("ListAlerts(t1, active) = %+v", t1active)
	}
	none, err := db.ListAlerts(ctx, "t9", false, 50)
	mustNoErr(t, err, "ListAlerts unknown tenant")
	if len(none) != 0 {
		t.Fatalf("unknown tenant returned %+v", none)
	}

	// Acknowledge through UpdateAlert with the tenant scope.
	ackAt := base.Add(10 * time.Minute)
	a := *got
	a.State = store.AlertStateAcknowledged
	a.AckedBy = "alice"
	a.AckedAt = ackAt
	a.CurrentTier = 1
	a.Retries = 2
	a.UpdatedAt = ackAt
	mustNoErr(t, db.UpdateAlert(ctx, "t1", &a), "UpdateAlert")
	got, err = db.GetAlert(ctx, "t1", "al-1")
	mustNoErr(t, err, "GetAlert after ack")
	if got.State != store.AlertStateAcknowledged || got.AckedBy != "alice" || !got.AckedAt.Equal(ackAt) || got.CurrentTier != 1 || got.Retries != 2 || !got.UpdatedAt.Equal(ackAt) {
		t.Fatalf("UpdateAlert not applied: %+v", got)
	}
	active, _ = db.ListAlerts(ctx, "", true, 100)
	if len(active) != 2 {
		t.Fatalf("acknowledged alert still active: %+v", active)
	}
	// Cross-tenant update is a no-op; wildcard tenant update applies.
	hijack := *got
	hijack.State = store.AlertStateExhausted
	mustNoErr(t, db.UpdateAlert(ctx, "t2", &hijack), "UpdateAlert cross-tenant")
	got, _ = db.GetAlert(ctx, "t1", "al-1")
	if got.State != store.AlertStateAcknowledged {
		t.Fatal("cross-tenant UpdateAlert leaked")
	}
	mustNoErr(t, db.UpdateAlert(ctx, "", &hijack), "UpdateAlert wildcard")
	got, _ = db.GetAlert(ctx, "t1", "al-1")
	if got.State != store.AlertStateExhausted {
		t.Fatal("wildcard UpdateAlert not applied")
	}
}

// --- Notification preferences ---

func TestNotificationPrefs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	p := &store.NotificationPref{DeviceIMEI: "3001", URLs: []string{"ntfy://hub/sos", "mailto://ops@example.com"}, Events: []string{"sos", "deadman"}, Enabled: true}
	mustNoErr(t, db.SaveNotificationPref(ctx, "t1", p), "SaveNotificationPref")
	mustNoErr(t, db.SaveNotificationPref(ctx, "t1", &store.NotificationPref{DeviceIMEI: "*", Enabled: false}), "SaveNotificationPref wildcard nil slices")
	mustNoErr(t, db.SaveNotificationPref(ctx, "t2", &store.NotificationPref{DeviceIMEI: "3001", URLs: []string{"slack://x"}, Events: []string{"mo"}, Enabled: true}), "SaveNotificationPref t2")

	got, err := db.GetNotificationPref(ctx, "t1", "3001")
	mustNoErr(t, err, "GetNotificationPref")
	if len(got.URLs) != 2 || got.URLs[1] != "mailto://ops@example.com" || len(got.Events) != 2 || got.Events[0] != "sos" || !got.Enabled {
		t.Fatalf("GetNotificationPref = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps: %+v", got)
	}
	wild, err := db.GetNotificationPref(ctx, "t1", "*")
	mustNoErr(t, err, "GetNotificationPref wildcard")
	if len(wild.URLs) != 0 || len(wild.Events) != 0 || wild.Enabled {
		t.Fatalf("nil slices must round-trip as empty: %+v", wild)
	}
	_, err = db.GetNotificationPref(ctx, "t3", "3001")
	wantNoRows(t, err, "GetNotificationPref cross-tenant")

	// Upsert on (device_imei, tenant_id): t1 row replaced, t2 untouched.
	mustNoErr(t, db.SaveNotificationPref(ctx, "t1", &store.NotificationPref{DeviceIMEI: "3001", URLs: []string{"ntfy://hub/all"}, Events: []string{"geofence"}, Enabled: false}), "SaveNotificationPref upsert")
	got, err = db.GetNotificationPref(ctx, "t1", "3001")
	mustNoErr(t, err, "GetNotificationPref after upsert")
	if len(got.URLs) != 1 || got.URLs[0] != "ntfy://hub/all" || len(got.Events) != 1 || got.Events[0] != "geofence" || got.Enabled {
		t.Fatalf("upsert not applied: %+v", got)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("updated_at %v before created_at %v", got.UpdatedAt, got.CreatedAt)
	}
	t2, err := db.GetNotificationPref(ctx, "t2", "3001")
	mustNoErr(t, err, "GetNotificationPref t2")
	if t2.URLs[0] != "slack://x" {
		t.Fatalf("t2 pref clobbered by t1 upsert: %+v", t2)
	}

	list, err := db.ListNotificationPrefs(ctx, "t1")
	mustNoErr(t, err, "ListNotificationPrefs")
	if len(list) != 2 || list[0].DeviceIMEI != "*" || list[1].DeviceIMEI != "3001" {
		t.Fatalf("ListNotificationPrefs (by device_imei) = %+v", list)
	}

	mustNoErr(t, db.DeleteNotificationPref(ctx, "t2", "*"), "DeleteNotificationPref cross-tenant")
	if _, err := db.GetNotificationPref(ctx, "t1", "*"); err != nil {
		t.Fatalf("cross-tenant delete removed pref: %v", err)
	}
	mustNoErr(t, db.DeleteNotificationPref(ctx, "t1", "3001"), "DeleteNotificationPref")
	_, err = db.GetNotificationPref(ctx, "t1", "3001")
	wantNoRows(t, err, "GetNotificationPref after delete")
	list, _ = db.ListNotificationPrefs(ctx, "t1")
	if len(list) != 1 {
		t.Fatalf("ListNotificationPrefs after delete = %+v", list)
	}
}
