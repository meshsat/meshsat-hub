package directory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Dialect is the SQL engine behind a SQLStore. The statements are the same
// on both (`$n` placeholders, which modernc's SQLite binds by number); the
// column types and the owner of the schema differ.
type Dialect int

const (
	// DialectSQLite is the standalone Hub. The store owns the schema
	// ([SQLStore.Migrate]).
	DialectSQLite Dialect = iota
	// DialectPostgres is production. The schema is migration 40 of
	// internal/store/postgres, applied by the Hub's migration runner under
	// its advisory lock, so replicas starting together never race on it.
	DialectPostgres
)

// SQLStore is the [Store] over database/sql, on SQLite or PostgreSQL.
//
// Every row carries its tenant_id and every key starts with it: a contact,
// group or policy id is unique within its tenant, an address within its
// tenant, and no statement reads or writes a row without naming the tenant
// (TestEveryDirectoryStatementNamesTheTenant). Each mutation that changes a
// row bumps the tenant's directory version in its own transaction, so a
// snapshot's version always describes exactly the rows it carries.
// [MESHSAT-538, MESHSAT-1584 C7]
type SQLStore struct {
	db      *sql.DB
	dialect Dialect
}

// NewSQLStore wraps a SQLite handle. The caller owns the connection and its
// lifecycle; the store does not close it. Open it with
// `_pragma=foreign_keys(1)`: modernc ignores the `_foreign_keys=ON` form.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db, dialect: DialectSQLite}
}

// NewPostgresStore wraps a PostgreSQL handle whose schema the Hub's store
// migrations created (internal/store/postgres, migration 40).
func NewPostgresStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db, dialect: DialectPostgres}
}

// querier is what a statement runs on: the pool or a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Migrate creates the SQLite directory schema, or upgrades the first one
// (ids and addresses unique across all tenants) in place. Idempotent, safe
// on every boot. On Postgres it does nothing: the store migrations own the
// schema there.
func (s *SQLStore) Migrate(ctx context.Context) error {
	if s.dialect == DialectPostgres {
		return nil
	}
	v1, err := s.sqliteHasV1Schema(ctx)
	if err != nil {
		return err
	}
	if v1 {
		return s.upgradeSQLiteV1(ctx)
	}
	for i, stmt := range append(append([]string{}, sqliteTables...), sqliteIndexes...) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("directory migration %d: %w", i+1, err)
		}
	}
	return nil
}

// precedenceDefault is one row SeedPrecedenceDefaults installs.
type precedenceDefault struct {
	scopeType string
	scopeID   string
	strategy  string
}

// PrecedenceDefaults are the STANAG 4406 precedence → dispatch
// strategy fall-backs installed per tenant by
// [SQLStore.SeedPrecedenceDefaults]. The set matches the Bridge's
// v48 migration so a fresh tenant on the Hub resolves identically
// to a fresh Bridge install. [MESHSAT-547]
var PrecedenceDefaults = []precedenceDefault{
	{"default", "", "PRIMARY_ONLY"},
	{"precedence", "Override", "HEMB_BONDED"},
	{"precedence", "Flash", "HEMB_BONDED"},
	{"precedence", "Immediate", "ANY_REACHABLE"},
	{"precedence", "Priority", "ORDERED_FALLBACK"},
	{"precedence", "Routine", "PRIMARY_ONLY"},
	{"precedence", "Deferred", "PRIMARY_ONLY"},
}

// SeedPrecedenceDefaults installs the per-tenant precedence → strategy
// fall-backs used by the dispatcher when no contact- or group-specific
// policy matches, marked seeded. A scope that already has a policy is left
// alone, decided by the (tenant, scope) key, so replicas seeding at the same
// time insert each default once. One transaction and one version bump when
// anything was inserted. Returns the number of rows inserted so callers can
// log first-boot provisioning. [MESHSAT-547 / S2-04]
func (s *SQLStore) SeedPrecedenceDefaults(ctx context.Context, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, ErrEmptyID
	}
	inserted := 0
	now := s.ts(storeNow())
	err := s.write(ctx, func(tx *sql.Tx) error {
		inserted = 0
		for _, d := range PrecedenceDefaults {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO directory_dispatch_policy
				(tenant_id, id, name, scope_type, scope_id, strategy, preferred, fallback,
				 require_encryption, max_retries, retry_delay_ns, seeded, created_at, updated_at)
				VALUES ($1, $2, '', $3, $4, $5, '[]', '[]', $6, 0, 0, $7, $8, $8)
				ON CONFLICT (tenant_id, scope_type, scope_id) DO NOTHING`,
				tenantID, NewID(), d.scopeType, d.scopeID, d.strategy, false, true, now)
			if err != nil {
				return fmt.Errorf("insert precedence default %s:%s: %w", d.scopeType, d.scopeID, err)
			}
			n, _ := res.RowsAffected()
			inserted += int(n)
		}
		if inserted == 0 {
			return nil
		}
		_, err := bumpVersion(ctx, tx, tenantID, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// sqliteTables is the SQLite directory DDL (CREATE … IF NOT EXISTS, safe on
// every boot). It mirrors migration 40 of internal/store/postgres column for
// column; only the types differ. Column names match the bridge-side v44-v48
// schema so Hub snapshots and bridge rows stay byte-compatible under JSON
// canonicalisation.
var sqliteTables = []string{
	`CREATE TABLE IF NOT EXISTS directory_contacts (
		tenant_id         TEXT NOT NULL,
		id                TEXT NOT NULL,
		display_name      TEXT NOT NULL,
		given_name        TEXT NOT NULL DEFAULT '',
		family_name       TEXT NOT NULL DEFAULT '',
		org               TEXT NOT NULL DEFAULT '',
		role              TEXT NOT NULL DEFAULT '',
		team              TEXT NOT NULL DEFAULT '',
		sidc              TEXT NOT NULL DEFAULT '',
		notes             TEXT NOT NULL DEFAULT '',
		trust_level       INTEGER NOT NULL DEFAULT 0,
		trust_verified_at TEXT,
		origin            TEXT NOT NULL DEFAULT 'hub',
		policy_id         TEXT NOT NULL DEFAULT '',
		hub_version       INTEGER NOT NULL DEFAULT 0,
		hub_etag          TEXT NOT NULL DEFAULT '',
		metadata          TEXT NOT NULL DEFAULT '{}',
		created_at        TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, id)
	)`,
	`CREATE TABLE IF NOT EXISTS directory_addresses (
		tenant_id      TEXT NOT NULL,
		id             TEXT NOT NULL,
		contact_id     TEXT NOT NULL,
		kind           TEXT NOT NULL,
		value          TEXT NOT NULL,
		subvalue       TEXT NOT NULL DEFAULT '',
		label          TEXT NOT NULL DEFAULT '',
		primary_rank   INTEGER NOT NULL DEFAULT 0,
		verified       INTEGER NOT NULL DEFAULT 0,
		bearer_hint    INTEGER NOT NULL DEFAULT 50,
		max_cost_cents INTEGER,
		created_at     TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, id),
		UNIQUE (tenant_id, kind, value),
		FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS directory_contact_keys (
		tenant_id    TEXT NOT NULL,
		id           TEXT NOT NULL,
		contact_id   TEXT NOT NULL,
		kind         TEXT NOT NULL,
		version      INTEGER NOT NULL DEFAULT 1,
		status       TEXT NOT NULL DEFAULT 'active',
		public_data  BLOB,
		fingerprint  TEXT NOT NULL DEFAULT '',
		algorithm    TEXT NOT NULL DEFAULT '',
		trust_anchor TEXT NOT NULL DEFAULT 'hub',
		created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, id),
		UNIQUE (tenant_id, contact_id, kind, version),
		FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS directory_groups (
		tenant_id    TEXT NOT NULL,
		id           TEXT NOT NULL,
		name         TEXT NOT NULL,
		kind         TEXT NOT NULL DEFAULT 'LIST',
		sidc         TEXT NOT NULL DEFAULT '',
		mls_group_id TEXT NOT NULL DEFAULT '',
		policy_id    TEXT NOT NULL DEFAULT '',
		hub_version  INTEGER NOT NULL DEFAULT 0,
		hub_etag     TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, id)
	)`,
	`CREATE TABLE IF NOT EXISTS directory_group_members (
		tenant_id  TEXT NOT NULL,
		group_id   TEXT NOT NULL,
		contact_id TEXT NOT NULL,
		position   INTEGER NOT NULL DEFAULT 0,
		role       TEXT NOT NULL DEFAULT '',
		added_at   TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, group_id, contact_id),
		FOREIGN KEY (tenant_id, group_id) REFERENCES directory_groups (tenant_id, id) ON DELETE CASCADE,
		FOREIGN KEY (tenant_id, contact_id) REFERENCES directory_contacts (tenant_id, id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS directory_dispatch_policy (
		tenant_id          TEXT NOT NULL,
		id                 TEXT NOT NULL,
		name               TEXT NOT NULL DEFAULT '',
		scope_type         TEXT NOT NULL DEFAULT 'contact',
		scope_id           TEXT NOT NULL DEFAULT '',
		strategy           TEXT NOT NULL DEFAULT 'PRIMARY_ONLY',
		preferred          TEXT NOT NULL DEFAULT '[]',
		fallback           TEXT NOT NULL DEFAULT '[]',
		require_encryption INTEGER NOT NULL DEFAULT 0,
		max_retries        INTEGER NOT NULL DEFAULT 0,
		retry_delay_ns     INTEGER NOT NULL DEFAULT 0,
		seeded             INTEGER NOT NULL DEFAULT 0,
		created_at         TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (tenant_id, id),
		UNIQUE (tenant_id, scope_type, scope_id)
	)`,
	`CREATE TABLE IF NOT EXISTS directory_versions (
		tenant_id  TEXT PRIMARY KEY,
		version    INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
}

// sqliteIndexes run after the tables (and, on an upgrade, after the old
// tables and the index names they held are gone).
var sqliteIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_dir_contacts_name ON directory_contacts (tenant_id, display_name)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_addr_contact ON directory_addresses (tenant_id, contact_id)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_members_contact ON directory_group_members (tenant_id, contact_id)`,
}

// sqliteHasV1Schema reports whether the first directory schema is in place:
// it had no tenant_id on the address table (ids and addresses were unique
// across every tenant, and an upsert by id could move a row to another one).
func (s *SQLStore) sqliteHasV1Schema(ctx context.Context) (bool, error) {
	var tables int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'directory_addresses'`).Scan(&tables); err != nil {
		return false, fmt.Errorf("directory schema probe: %w", err)
	}
	if tables == 0 {
		return false, nil
	}
	var cols int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('directory_addresses') WHERE name = 'tenant_id'`).Scan(&cols); err != nil {
		return false, fmt.Errorf("directory schema probe: %w", err)
	}
	return cols == 0, nil
}

// upgradeSQLiteV1 rebuilds the first schema as the tenant-keyed one in one
// transaction: rename the old tables, create the new ones, copy every row
// with its tenant (a child takes its parent's), drop the old tables, create
// the indexes. Two things the old schema allowed are not copied: a group
// membership across tenants, and a second policy for a (tenant, scope) that
// already has one (the oldest stays). directory_versions keeps its shape and
// its rows.
func (s *SQLStore) upgradeSQLiteV1(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		steps := []string{
			`ALTER TABLE directory_group_members RENAME TO directory_group_members_v1`,
			`ALTER TABLE directory_contact_keys RENAME TO directory_contact_keys_v1`,
			`ALTER TABLE directory_addresses RENAME TO directory_addresses_v1`,
			`ALTER TABLE directory_groups RENAME TO directory_groups_v1`,
			`ALTER TABLE directory_contacts RENAME TO directory_contacts_v1`,
			`ALTER TABLE directory_dispatch_policy RENAME TO directory_dispatch_policy_v1`,
		}
		steps = append(steps, sqliteTables...)
		steps = append(steps,
			`INSERT INTO directory_contacts (tenant_id, id, display_name, given_name, family_name, org, role, team,
				sidc, notes, trust_level, trust_verified_at, origin, policy_id, hub_version, hub_etag, metadata,
				created_at, updated_at)
			SELECT tenant_id, id, display_name, given_name, family_name, org, role, team,
				sidc, notes, trust_level, trust_verified_at, origin, policy_id, hub_version, hub_etag, metadata,
				created_at, updated_at
			FROM directory_contacts_v1`,
			`INSERT INTO directory_addresses (tenant_id, id, contact_id, kind, value, subvalue, label, primary_rank,
				verified, bearer_hint, max_cost_cents, created_at, updated_at)
			SELECT c.tenant_id, a.id, a.contact_id, a.kind, a.value, a.subvalue, a.label, a.primary_rank,
				a.verified, a.bearer_hint, a.max_cost_cents, a.created_at, a.updated_at
			FROM directory_addresses_v1 a JOIN directory_contacts_v1 c ON c.id = a.contact_id`,
			`INSERT INTO directory_contact_keys (tenant_id, id, contact_id, kind, version, status, public_data,
				fingerprint, algorithm, trust_anchor, created_at)
			SELECT c.tenant_id, k.id, k.contact_id, k.kind, k.version, k.status, k.public_data,
				k.fingerprint, k.algorithm, k.trust_anchor, k.created_at
			FROM directory_contact_keys_v1 k JOIN directory_contacts_v1 c ON c.id = k.contact_id`,
			`INSERT INTO directory_groups (tenant_id, id, name, kind, sidc, mls_group_id, policy_id, hub_version,
				hub_etag, created_at, updated_at)
			SELECT tenant_id, id, name, kind, sidc, mls_group_id, policy_id, hub_version,
				hub_etag, created_at, updated_at
			FROM directory_groups_v1`,
			`INSERT INTO directory_group_members (tenant_id, group_id, contact_id, position, role, added_at)
			SELECT g.tenant_id, m.group_id, m.contact_id, m.rowid, m.role, m.added_at
			FROM directory_group_members_v1 m
			JOIN directory_groups_v1 g ON g.id = m.group_id
			JOIN directory_contacts_v1 c ON c.id = m.contact_id AND c.tenant_id = g.tenant_id`,
			`INSERT OR IGNORE INTO directory_dispatch_policy (tenant_id, id, name, scope_type, scope_id, strategy,
				preferred, fallback, require_encryption, max_retries, retry_delay_ns, seeded, created_at, updated_at)
			SELECT tenant_id, id, name, scope_type, scope_id, strategy,
				preferred, fallback, require_encryption, max_retries, retry_delay_ns, 0, created_at, updated_at
			FROM directory_dispatch_policy_v1 ORDER BY created_at, rowid`,
			`DROP TABLE directory_group_members_v1`,
			`DROP TABLE directory_contact_keys_v1`,
			`DROP TABLE directory_addresses_v1`,
			`DROP TABLE directory_groups_v1`,
			`DROP TABLE directory_contacts_v1`,
			`DROP TABLE directory_dispatch_policy_v1`,
		)
		steps = append(steps, sqliteIndexes...)
		for i, stmt := range steps {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("directory upgrade step %d: %w", i+1, err)
			}
		}
		return markSeededV1(ctx, tx)
	})
}

// markSeededV1 marks the copied policies that are exactly what the seed
// wrote (the first schema did not record it), per tenant.
func markSeededV1(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT tenant_id FROM directory_dispatch_policy`)
	if err != nil {
		return err
	}
	var tenants []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			_ = rows.Close()
			return err
		}
		tenants = append(tenants, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range tenants {
		for _, d := range PrecedenceDefaults {
			if _, err := tx.ExecContext(ctx, `
				UPDATE directory_dispatch_policy SET seeded = 1
				WHERE tenant_id = $1 AND scope_type = $2 AND scope_id = $3 AND strategy = $4 AND name = ''
				  AND preferred = '[]' AND fallback = '[]' AND require_encryption = 0
				  AND max_retries = 0 AND retry_delay_ns = 0`,
				t, d.scopeType, d.scopeID, d.strategy); err != nil {
				return err
			}
		}
	}
	return nil
}

// NewID returns a UUIDv4 string suitable for any directory_* PK.
func NewID() string { return uuid.NewString() }

// storeNow is the store's clock: UTC, whole seconds on both drivers, so a
// row reads back the same from either.
func storeNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

// ts is a time as the driver stores it: TIMESTAMPTZ on Postgres, the
// `datetime('now')` text form on SQLite.
func (s *SQLStore) ts(t time.Time) any {
	if s.dialect == DialectPostgres {
		return t.UTC()
	}
	return t.UTC().Format(time.DateTime)
}

// write runs fn in one transaction and commits it when fn returns nil.
func (s *SQLStore) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// bumpVersion raises the tenant's directory version by one inside the
// caller's transaction and returns it. The row lock it takes orders
// concurrent writers of one tenant; it is every write's last statement.
func bumpVersion(ctx context.Context, q querier, tenantID string, at any) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO directory_versions (tenant_id, version, updated_at) VALUES ($1, 1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET version = directory_versions.version + 1, updated_at = excluded.updated_at
		RETURNING version`, tenantID, at).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("bump directory version: %w", err)
	}
	return v, nil
}

// --- Contacts ----------------------------------------------------------

func (s *SQLStore) GetContact(ctx context.Context, tenantID, contactID string) (*Contact, error) {
	if tenantID == "" || contactID == "" {
		return nil, ErrEmptyID
	}
	return getContact(ctx, s.db, tenantID, contactID)
}

func getContact(ctx context.Context, q querier, tenantID, contactID string) (*Contact, error) {
	c, err := scanContact(q.QueryRowContext(ctx, `
		SELECT id, tenant_id, display_name, given_name, family_name, org, role, team,
		       sidc, notes, trust_level, trust_verified_at, origin, policy_id,
		       hub_version, hub_etag, metadata, created_at, updated_at
		FROM directory_contacts WHERE tenant_id = $1 AND id = $2`, tenantID, contactID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}
	addrs, err := loadAddresses(ctx, q, tenantID, contactID)
	if err != nil {
		return nil, err
	}
	keys, err := loadKeys(ctx, q, tenantID, contactID)
	if err != nil {
		return nil, err
	}
	gids, err := loadContactGroups(ctx, q, tenantID, contactID)
	if err != nil {
		return nil, err
	}
	c.Addresses, c.Keys, c.GroupIDs = addrs[contactID], keys[contactID], gids[contactID]
	return c, nil
}

func (s *SQLStore) ListContacts(ctx context.Context, tenantID string) ([]Contact, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	return listContacts(ctx, s.db, tenantID)
}

func listContacts(ctx context.Context, q querier, tenantID string) ([]Contact, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, tenant_id, display_name, given_name, family_name, org, role, team,
		       sidc, notes, trust_level, trust_verified_at, origin, policy_id,
		       hub_version, hub_etag, metadata, created_at, updated_at
		FROM directory_contacts WHERE tenant_id = $1 ORDER BY display_name ASC, id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, *c)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	addrs, err := loadAddresses(ctx, q, tenantID, "")
	if err != nil {
		return nil, err
	}
	keys, err := loadKeys(ctx, q, tenantID, "")
	if err != nil {
		return nil, err
	}
	gids, err := loadContactGroups(ctx, q, tenantID, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		id := out[i].ID
		out[i].Addresses, out[i].Keys, out[i].GroupIDs = addrs[id], keys[id], gids[id]
	}
	return out, nil
}

func (s *SQLStore) FindContactByAddress(ctx context.Context, tenantID string, kind AddressKind, value string) (*Contact, error) {
	if tenantID == "" || value == "" {
		return nil, ErrEmptyID
	}
	if !kind.Valid() {
		return nil, ErrInvalidAddressKind
	}
	value, err := kind.Canonical(value)
	if err != nil {
		return nil, err
	}
	var cid string
	err = s.db.QueryRowContext(ctx, `
		SELECT contact_id FROM directory_addresses
		WHERE tenant_id = $1 AND kind = $2 AND value = $3`,
		tenantID, string(kind), value).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find by address: %w", err)
	}
	return s.GetContact(ctx, tenantID, cid)
}

// PutContact inserts or updates (by tenant and ID) a contact and replaces
// its addresses and keys, all in one transaction with the version bump.
// Address values are stored in their kind's canonical form
// (AddressKind.Canonical). An address another contact of the tenant holds
// fails the whole put with ErrAddressTaken and changes nothing.
func (s *SQLStore) PutContact(ctx context.Context, c *Contact) error {
	if c == nil {
		return fmt.Errorf("%w: nil contact", ErrEmptyID)
	}
	if c.TenantID == "" || c.DisplayName == "" {
		return fmt.Errorf("%w: tenant_id and display_name are required", ErrEmptyID)
	}
	for i := range c.Addresses {
		a := &c.Addresses[i]
		if !a.Kind.Valid() {
			return fmt.Errorf("%w: %s", ErrInvalidAddressKind, a.Kind)
		}
		v, err := a.Kind.Canonical(a.Value)
		if err != nil {
			return err
		}
		a.Value = v
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.Origin == "" {
		c.Origin = OriginHub
	}
	now := storeNow()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	metaJSON, err := encodeMetadata(c.Metadata)
	if err != nil {
		return err
	}
	var verifiedAt any
	if c.TrustVerifiedAt != nil {
		verifiedAt = s.ts(*c.TrustVerifiedAt)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var created dbTime
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO directory_contacts (
				tenant_id, id, display_name, given_name, family_name, org, role, team,
				sidc, notes, trust_level, trust_verified_at, origin, policy_id,
				hub_version, hub_etag, metadata, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				display_name = excluded.display_name,
				given_name = excluded.given_name,
				family_name = excluded.family_name,
				org = excluded.org, role = excluded.role, team = excluded.team,
				sidc = excluded.sidc, notes = excluded.notes,
				trust_level = excluded.trust_level,
				trust_verified_at = excluded.trust_verified_at,
				origin = excluded.origin, policy_id = excluded.policy_id,
				hub_version = excluded.hub_version, hub_etag = excluded.hub_etag,
				metadata = excluded.metadata, updated_at = excluded.updated_at
			RETURNING created_at`,
			c.TenantID, c.ID, c.DisplayName, c.GivenName, c.FamilyName, c.Org, c.Role, c.Team,
			c.SIDC, c.Notes, int(c.TrustLevel), verifiedAt, string(c.Origin), c.PolicyID,
			c.HubVersion, c.HubEtag, metaJSON, s.ts(c.CreatedAt), s.ts(c.UpdatedAt),
		).Scan(&created); err != nil {
			return fmt.Errorf("put contact: %w", err)
		}
		if created.valid {
			c.CreatedAt = created.t
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM directory_addresses WHERE tenant_id = $1 AND contact_id = $2`, c.TenantID, c.ID); err != nil {
			return fmt.Errorf("reset addresses: %w", err)
		}
		for i := range c.Addresses {
			a := &c.Addresses[i]
			if a.ID == "" {
				a.ID = NewID()
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO directory_addresses
				(tenant_id, id, contact_id, kind, value, subvalue, label, primary_rank, verified, bearer_hint,
				 max_cost_cents, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
				ON CONFLICT DO NOTHING`,
				c.TenantID, a.ID, c.ID, string(a.Kind), a.Value, a.Subvalue, a.Label,
				a.PrimaryRank, a.Verified, a.BearerHint, a.MaxCostCents, s.ts(now))
			if err != nil {
				return fmt.Errorf("put address %s: %w", a.Value, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: %s %s", ErrAddressTaken, a.Kind, a.Value)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM directory_contact_keys WHERE tenant_id = $1 AND contact_id = $2`, c.TenantID, c.ID); err != nil {
			return fmt.Errorf("reset keys: %w", err)
		}
		for i := range c.Keys {
			k := &c.Keys[i]
			if k.ID == "" {
				k.ID = NewID()
			}
			if k.Status == "" {
				k.Status = "active"
			}
			if k.Version == 0 {
				k.Version = 1
			}
			if k.CreatedAt.IsZero() {
				k.CreatedAt = now
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO directory_contact_keys
				(tenant_id, id, contact_id, kind, version, status, public_data, fingerprint, algorithm,
				 trust_anchor, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT DO NOTHING`,
				c.TenantID, k.ID, c.ID, k.Kind, k.Version, k.Status, k.Public, k.Fingerprint, k.Algorithm,
				k.TrustAnchor, s.ts(k.CreatedAt))
			if err != nil {
				return fmt.Errorf("put key %s: %w", k.ID, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: %s", ErrKeyTaken, k.ID)
			}
		}
		_, err := bumpVersion(ctx, tx, c.TenantID, s.ts(now))
		return err
	})
}

// DeleteContact removes a contact, its addresses, keys and group
// memberships, and bumps the version. The children are deleted by name, not
// left to the cascade alone, so a SQLite handle opened without foreign keys
// leaves nothing behind either.
func (s *SQLStore) DeleteContact(ctx context.Context, tenantID, contactID string) error {
	if tenantID == "" || contactID == "" {
		return ErrEmptyID
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM directory_group_members WHERE tenant_id = $1 AND contact_id = $2`,
			`DELETE FROM directory_addresses WHERE tenant_id = $1 AND contact_id = $2`,
			`DELETE FROM directory_contact_keys WHERE tenant_id = $1 AND contact_id = $2`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, tenantID, contactID); err != nil {
				return fmt.Errorf("delete contact: %w", err)
			}
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM directory_contacts WHERE tenant_id = $1 AND id = $2`, tenantID, contactID)
		if err != nil {
			return fmt.Errorf("delete contact: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = bumpVersion(ctx, tx, tenantID, s.ts(storeNow()))
		return err
	})
}

// --- Groups ------------------------------------------------------------

func (s *SQLStore) GetGroup(ctx context.Context, tenantID, groupID string) (*Group, error) {
	if tenantID == "" || groupID == "" {
		return nil, ErrEmptyID
	}
	g, err := scanGroup(s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, kind, sidc, mls_group_id, policy_id,
		       hub_version, hub_etag, created_at, updated_at
		FROM directory_groups WHERE tenant_id = $1 AND id = $2`, tenantID, groupID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}
	members, err := loadGroupMembers(ctx, s.db, tenantID, groupID)
	if err != nil {
		return nil, err
	}
	g.MemberIDs = members[groupID]
	return g, nil
}

func (s *SQLStore) ListGroups(ctx context.Context, tenantID string) ([]Group, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	return listGroups(ctx, s.db, tenantID)
}

func listGroups(ctx context.Context, q querier, tenantID string) ([]Group, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, tenant_id, name, kind, sidc, mls_group_id, policy_id,
		       hub_version, hub_etag, created_at, updated_at
		FROM directory_groups WHERE tenant_id = $1 ORDER BY name ASC, id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, *g)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	members, err := loadGroupMembers(ctx, q, tenantID, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].MemberIDs = members[out[i].ID]
	}
	return out, nil
}

// PutGroup inserts or updates (by tenant and ID) a group and sets its
// members in the order given (a repeated id counts once). The kind is one
// of the four GroupKinds (LIST when empty). Every member must be a contact
// of the same tenant, or the put fails with ErrUnknownMember and changes
// nothing. A member that stays keeps the time it was added.
func (s *SQLStore) PutGroup(ctx context.Context, g *Group) error {
	if g == nil || g.TenantID == "" || g.Name == "" {
		return fmt.Errorf("%w: tenant_id and name required", ErrEmptyID)
	}
	if g.Kind == "" {
		g.Kind = GroupList
	}
	if !g.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidGroupKind, g.Kind)
	}
	if g.ID == "" {
		g.ID = NewID()
	}
	members := make([]string, 0, len(g.MemberIDs))
	seen := map[string]bool{}
	for _, id := range g.MemberIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		members = append(members, id)
	}
	now := storeNow()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	err := s.write(ctx, func(tx *sql.Tx) error {
		var created dbTime
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO directory_groups (tenant_id, id, name, kind, sidc, mls_group_id, policy_id, hub_version,
				hub_etag, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				name = excluded.name, kind = excluded.kind,
				sidc = excluded.sidc, mls_group_id = excluded.mls_group_id,
				policy_id = excluded.policy_id, hub_version = excluded.hub_version,
				hub_etag = excluded.hub_etag, updated_at = excluded.updated_at
			RETURNING created_at`,
			g.TenantID, g.ID, g.Name, string(g.Kind), g.SIDC, g.MLSGroupID, g.PolicyID,
			g.HubVersion, g.HubEtag, s.ts(g.CreatedAt), s.ts(g.UpdatedAt)).Scan(&created); err != nil {
			return fmt.Errorf("put group: %w", err)
		}
		if created.valid {
			g.CreatedAt = created.t
		}
		added, err := memberAddedAt(ctx, tx, g.TenantID, g.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM directory_group_members WHERE tenant_id = $1 AND group_id = $2`, g.TenantID, g.ID); err != nil {
			return fmt.Errorf("reset members: %w", err)
		}
		for pos, cid := range members {
			var one int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM directory_contacts WHERE tenant_id = $1 AND id = $2`, g.TenantID, cid).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrUnknownMember, cid)
			}
			if err != nil {
				return fmt.Errorf("check member %s: %w", cid, err)
			}
			at := s.ts(now)
			if t, ok := added[cid]; ok {
				at = s.ts(t)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO directory_group_members (tenant_id, group_id, contact_id, position, role, added_at)
				VALUES ($1, $2, $3, $4, '', $5)`, g.TenantID, g.ID, cid, pos, at); err != nil {
				return fmt.Errorf("add member %s: %w", cid, err)
			}
		}
		_, err = bumpVersion(ctx, tx, g.TenantID, s.ts(now))
		return err
	})
	if err != nil {
		return err
	}
	g.MemberIDs = members
	return nil
}

// memberAddedAt reads when each current member of a group was added.
func memberAddedAt(ctx context.Context, q querier, tenantID, groupID string) (map[string]time.Time, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT contact_id, added_at FROM directory_group_members WHERE tenant_id = $1 AND group_id = $2`,
		tenantID, groupID)
	if err != nil {
		return nil, fmt.Errorf("read members: %w", err)
	}
	out := map[string]time.Time{}
	for rows.Next() {
		var cid string
		var at dbTime
		if err := rows.Scan(&cid, &at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if at.valid {
			out[cid] = at.t
		}
	}
	return out, closeRows(rows)
}

// DeleteGroup removes a group and its memberships and bumps the version.
func (s *SQLStore) DeleteGroup(ctx context.Context, tenantID, groupID string) error {
	if tenantID == "" || groupID == "" {
		return ErrEmptyID
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM directory_group_members WHERE tenant_id = $1 AND group_id = $2`, tenantID, groupID); err != nil {
			return fmt.Errorf("delete group: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM directory_groups WHERE tenant_id = $1 AND id = $2`, tenantID, groupID)
		if err != nil {
			return fmt.Errorf("delete group: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = bumpVersion(ctx, tx, tenantID, s.ts(storeNow()))
		return err
	})
}

// --- Policies ----------------------------------------------------------

func (s *SQLStore) GetPolicy(ctx context.Context, tenantID, policyID string) (*DispatchPolicy, error) {
	if tenantID == "" || policyID == "" {
		return nil, ErrEmptyID
	}
	p, err := scanPolicy(s.db.QueryRowContext(ctx, `
		SELECT tenant_id, id, name, scope_type, scope_id, strategy, preferred, fallback,
		       require_encryption, max_retries, retry_delay_ns, seeded, created_at, updated_at
		FROM directory_dispatch_policy WHERE tenant_id = $1 AND id = $2`, tenantID, policyID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get policy: %w", err)
	}
	return p, nil
}

func (s *SQLStore) ListPolicies(ctx context.Context, tenantID string) ([]DispatchPolicy, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	return listPolicies(ctx, s.db, tenantID)
}

func listPolicies(ctx context.Context, q querier, tenantID string) ([]DispatchPolicy, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT tenant_id, id, name, scope_type, scope_id, strategy, preferred, fallback,
		       require_encryption, max_retries, retry_delay_ns, seeded, created_at, updated_at
		FROM directory_dispatch_policy WHERE tenant_id = $1
		ORDER BY scope_type ASC, scope_id ASC, id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	var out []DispatchPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, *p)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	return out, nil
}

// PutPolicy inserts or updates (by tenant and ID) a dispatch policy and
// bumps the version. p.TenantID is required: a policy belongs to exactly one
// tenant. A policy outside the model fails with ErrInvalidPolicy
// (DispatchPolicy.Check). A (scope_type, scope_id) another policy of the
// tenant covers fails with ErrPolicyScopeTaken. A put is an operator's
// word, so it clears seeded.
func (s *SQLStore) PutPolicy(ctx context.Context, p *DispatchPolicy) error {
	if p == nil {
		return fmt.Errorf("%w: nil policy", ErrEmptyID)
	}
	if p.TenantID == "" {
		return fmt.Errorf("%w: tenant_id is required", ErrEmptyID)
	}
	if err := p.Check(); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	now := storeNow()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	preferredJSON, err := json.Marshal(p.Preferred)
	if err != nil {
		return err
	}
	fallbackJSON, err := json.Marshal(p.Fallback)
	if err != nil {
		return err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var other string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM directory_dispatch_policy
			WHERE tenant_id = $1 AND scope_type = $2 AND scope_id = $3 AND id <> $4`,
			p.TenantID, p.ScopeType, p.ScopeID, p.ID).Scan(&other)
		if err == nil {
			return fmt.Errorf("%w: %s:%s is policy %s", ErrPolicyScopeTaken, p.ScopeType, p.ScopeID, other)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check policy scope: %w", err)
		}
		var created dbTime
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO directory_dispatch_policy
			(tenant_id, id, name, scope_type, scope_id, strategy, preferred, fallback,
			 require_encryption, max_retries, retry_delay_ns, seeded, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				name = excluded.name,
				scope_type = excluded.scope_type, scope_id = excluded.scope_id,
				strategy = excluded.strategy, preferred = excluded.preferred,
				fallback = excluded.fallback, require_encryption = excluded.require_encryption,
				max_retries = excluded.max_retries, retry_delay_ns = excluded.retry_delay_ns,
				seeded = excluded.seeded, updated_at = excluded.updated_at
			RETURNING created_at`,
			p.TenantID, p.ID, p.Name, p.ScopeType, p.ScopeID, string(p.Strategy),
			string(preferredJSON), string(fallbackJSON),
			p.RequireEncryption, p.MaxRetries, int64(p.RetryDelay), false,
			s.ts(p.CreatedAt), s.ts(p.UpdatedAt),
		).Scan(&created); err != nil {
			return fmt.Errorf("put policy: %w", err)
		}
		if created.valid {
			p.CreatedAt = created.t
		}
		_, err = bumpVersion(ctx, tx, p.TenantID, s.ts(now))
		return err
	})
	if err != nil {
		return err
	}
	p.Seeded = false
	return nil
}

// DeletePolicy removes a dispatch policy and bumps the version.
func (s *SQLStore) DeletePolicy(ctx context.Context, tenantID, policyID string) error {
	if tenantID == "" || policyID == "" {
		return ErrEmptyID
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM directory_dispatch_policy WHERE tenant_id = $1 AND id = $2`, tenantID, policyID)
		if err != nil {
			return fmt.Errorf("delete policy: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = bumpVersion(ctx, tx, tenantID, s.ts(storeNow()))
		return err
	})
}

// --- Versioning + snapshot --------------------------------------------

func (s *SQLStore) CurrentVersion(ctx context.Context, tenantID string) (int64, error) {
	if tenantID == "" {
		return 0, ErrEmptyID
	}
	return currentVersion(ctx, s.db, tenantID)
}

func currentVersion(ctx context.Context, q querier, tenantID string) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx,
		`SELECT version FROM directory_versions WHERE tenant_id = $1`, tenantID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("directory version: %w", err)
	}
	return v, nil
}

// Snapshot reads the tenant's contacts, groups, policies and version in one
// read-only transaction (REPEATABLE READ on Postgres; one SQLite read
// transaction), so the version names exactly the rows returned.
func (s *SQLStore) Snapshot(ctx context.Context, tenantID string) (*Snapshot, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version, err := currentVersion(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	contacts, err := listContacts(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	groups, err := listGroups(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	policies, err := listPolicies(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return &Snapshot{
		TenantID: tenantID,
		Version:  version,
		SignedAt: time.Now().UTC(),
		Contacts: contacts,
		Groups:   groups,
		Policies: policies,
	}, nil
}

// --- internal helpers --------------------------------------------------

// loadAddresses returns the tenant's addresses by contact id: of one contact,
// or of all when contactID is empty.
func loadAddresses(ctx context.Context, q querier, tenantID, contactID string) (map[string][]Address, error) {
	var rows *sql.Rows
	var err error
	if contactID == "" {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, id, kind, value, subvalue, label, primary_rank, verified, bearer_hint, max_cost_cents
			FROM directory_addresses WHERE tenant_id = $1
			ORDER BY contact_id ASC, primary_rank ASC, kind ASC, value ASC, id ASC`, tenantID)
	} else {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, id, kind, value, subvalue, label, primary_rank, verified, bearer_hint, max_cost_cents
			FROM directory_addresses WHERE tenant_id = $1 AND contact_id = $2
			ORDER BY primary_rank ASC, kind ASC, value ASC, id ASC`, tenantID, contactID)
	}
	if err != nil {
		return nil, fmt.Errorf("load addresses: %w", err)
	}
	out := map[string][]Address{}
	for rows.Next() {
		var a Address
		var cid, kind string
		var maxCost sql.NullInt64
		if err := rows.Scan(&cid, &a.ID, &kind, &a.Value, &a.Subvalue, &a.Label, &a.PrimaryRank, &a.Verified,
			&a.BearerHint, &maxCost); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("load addresses: %w", err)
		}
		a.Kind = AddressKind(kind)
		if maxCost.Valid {
			v := int(maxCost.Int64)
			a.MaxCostCents = &v
		}
		out[cid] = append(out[cid], a)
	}
	return out, closeRows(rows)
}

// loadKeys returns the tenant's contact keys by contact id: of one contact,
// or of all when contactID is empty.
func loadKeys(ctx context.Context, q querier, tenantID, contactID string) (map[string][]ContactKey, error) {
	var rows *sql.Rows
	var err error
	if contactID == "" {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, id, kind, version, status, public_data, fingerprint, algorithm, trust_anchor, created_at
			FROM directory_contact_keys WHERE tenant_id = $1
			ORDER BY contact_id ASC, kind ASC, version DESC, id ASC`, tenantID)
	} else {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, id, kind, version, status, public_data, fingerprint, algorithm, trust_anchor, created_at
			FROM directory_contact_keys WHERE tenant_id = $1 AND contact_id = $2
			ORDER BY kind ASC, version DESC, id ASC`, tenantID, contactID)
	}
	if err != nil {
		return nil, fmt.Errorf("load keys: %w", err)
	}
	out := map[string][]ContactKey{}
	for rows.Next() {
		var k ContactKey
		var cid string
		var created dbTime
		if err := rows.Scan(&cid, &k.ID, &k.Kind, &k.Version, &k.Status, &k.Public, &k.Fingerprint, &k.Algorithm,
			&k.TrustAnchor, &created); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("load keys: %w", err)
		}
		k.CreatedAt = created.t
		out[cid] = append(out[cid], k)
	}
	return out, closeRows(rows)
}

// loadContactGroups returns, by contact id, the groups each contact belongs
// to in the order it joined them (of one contact, or of all when contactID
// is empty). A contact's policy inherits from the first group that has one.
func loadContactGroups(ctx context.Context, q querier, tenantID, contactID string) (map[string][]string, error) {
	var rows *sql.Rows
	var err error
	if contactID == "" {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, group_id FROM directory_group_members WHERE tenant_id = $1
			ORDER BY contact_id ASC, added_at ASC, group_id ASC`, tenantID)
	} else {
		rows, err = q.QueryContext(ctx, `
			SELECT contact_id, group_id FROM directory_group_members WHERE tenant_id = $1 AND contact_id = $2
			ORDER BY added_at ASC, group_id ASC`, tenantID, contactID)
	}
	if err != nil {
		return nil, fmt.Errorf("load memberships: %w", err)
	}
	return scanPairs(rows)
}

// loadGroupMembers returns, by group id, each group's members in the order
// the group was given them (of one group, or of all when groupID is empty).
func loadGroupMembers(ctx context.Context, q querier, tenantID, groupID string) (map[string][]string, error) {
	var rows *sql.Rows
	var err error
	if groupID == "" {
		rows, err = q.QueryContext(ctx, `
			SELECT group_id, contact_id FROM directory_group_members WHERE tenant_id = $1
			ORDER BY group_id ASC, position ASC, contact_id ASC`, tenantID)
	} else {
		rows, err = q.QueryContext(ctx, `
			SELECT group_id, contact_id FROM directory_group_members WHERE tenant_id = $1 AND group_id = $2
			ORDER BY position ASC, contact_id ASC`, tenantID, groupID)
	}
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	return scanPairs(rows)
}

// scanPairs reads (key, value) rows into ordered lists by key.
func scanPairs(rows *sql.Rows) (map[string][]string, error) {
	out := map[string][]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[k] = append(out[k], v)
	}
	return out, closeRows(rows)
}

// closeRows closes rows and reports the iteration error, if any.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// rowScanner is the narrow scan contract shared by *sql.Row and
// *sql.Rows (both expose Scan(dest ...any) error).
type rowScanner interface {
	Scan(dest ...any) error
}

// dbTime scans a stored time from either driver: a time.Time from Postgres,
// the `datetime('now')` text (or RFC 3339) from SQLite, NULL as invalid.
type dbTime struct {
	t     time.Time
	valid bool
}

func (d *dbTime) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*d = dbTime{}
		return nil
	case time.Time:
		d.t, d.valid = x.UTC(), true
		return nil
	case string:
		return d.parse(x)
	case []byte:
		return d.parse(string(x))
	}
	return fmt.Errorf("directory: unsupported time value %T", v)
}

func (d *dbTime) parse(s string) error {
	for _, layout := range []string{time.DateTime, time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			d.t, d.valid = t.UTC(), true
			return nil
		}
	}
	return fmt.Errorf("directory: unparseable time %q", s)
}

func scanContact(r rowScanner) (*Contact, error) {
	var (
		c          Contact
		origin     string
		verifiedAt dbTime
		metaJSON   string
		createdAt  dbTime
		updatedAt  dbTime
		trustLevel int
	)
	err := r.Scan(&c.ID, &c.TenantID, &c.DisplayName, &c.GivenName, &c.FamilyName, &c.Org,
		&c.Role, &c.Team, &c.SIDC, &c.Notes, &trustLevel, &verifiedAt, &origin, &c.PolicyID,
		&c.HubVersion, &c.HubEtag, &metaJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	c.TrustLevel = TrustLevel(trustLevel)
	c.Origin = Origin(origin)
	if verifiedAt.valid {
		t := verifiedAt.t
		c.TrustVerifiedAt = &t
	}
	if metaJSON != "" && metaJSON != "{}" {
		if err := json.Unmarshal([]byte(metaJSON), &c.Metadata); err != nil {
			return nil, fmt.Errorf("contact %s metadata: %w", c.ID, err)
		}
	}
	c.CreatedAt, c.UpdatedAt = createdAt.t, updatedAt.t
	return &c, nil
}

func scanGroup(r rowScanner) (*Group, error) {
	var (
		g         Group
		createdAt dbTime
		updatedAt dbTime
	)
	err := r.Scan(&g.ID, &g.TenantID, &g.Name, &g.Kind, &g.SIDC, &g.MLSGroupID, &g.PolicyID,
		&g.HubVersion, &g.HubEtag, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	g.CreatedAt, g.UpdatedAt = createdAt.t, updatedAt.t
	return &g, nil
}

func scanPolicy(r rowScanner) (*DispatchPolicy, error) {
	var (
		p             DispatchPolicy
		strategy      string
		preferredJSON string
		fallbackJSON  string
		retryDelayNs  int64
		createdAt     dbTime
		updatedAt     dbTime
	)
	err := r.Scan(&p.TenantID, &p.ID, &p.Name, &p.ScopeType, &p.ScopeID, &strategy, &preferredJSON, &fallbackJSON,
		&p.RequireEncryption, &p.MaxRetries, &retryDelayNs, &p.Seeded, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.Strategy = Strategy(strategy)
	p.RetryDelay = time.Duration(retryDelayNs)
	if preferredJSON != "" {
		if err := json.Unmarshal([]byte(preferredJSON), &p.Preferred); err != nil {
			return nil, fmt.Errorf("policy %s preferred: %w", p.ID, err)
		}
	}
	if fallbackJSON != "" {
		if err := json.Unmarshal([]byte(fallbackJSON), &p.Fallback); err != nil {
			return nil, fmt.Errorf("policy %s fallback: %w", p.ID, err)
		}
	}
	p.CreatedAt, p.UpdatedAt = createdAt.t, updatedAt.t
	return &p, nil
}

func encodeMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// compile-time interface check (keeps Resolver compatibility intact).
var _ Store = (*SQLStore)(nil)
