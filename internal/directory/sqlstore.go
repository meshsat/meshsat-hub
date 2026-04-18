package directory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SQLStore is a concrete [Store] implementation over database/sql
// (SQLite or MariaDB — both speak the same DDL subset used below,
// with the caveat that MariaDB needs `VARCHAR(N)` where SQLite uses
// plain TEXT; Migrate emits SQLite DDL. A thin MariaDB variant can
// be added later if the Hub's cluster mode needs it.)
//
// All tenant-scoped methods take a tenantID and must not return
// records from other tenants. [MESHSAT-538]
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore wraps an existing *sql.DB handle. The caller owns the
// connection and its lifecycle; the store does not close it.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// Migrate creates the seven directory tables if they do not already
// exist. Idempotent — safe to call on every boot.
func (s *SQLStore) Migrate(ctx context.Context) error {
	for i, stmt := range schemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("directory migration %d: %w", i+1, err)
		}
	}
	return nil
}

// schemaStatements is the directory DDL. Everything uses CREATE …
// IF NOT EXISTS so it is safe to run every boot. Column types match
// the bridge-side v44-v48 schema so Hub snapshots and bridge rows
// stay byte-compatible under JSON canonicalisation.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS directory_contacts (
		id                TEXT PRIMARY KEY,
		tenant_id         TEXT NOT NULL DEFAULT '',
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
		updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_contacts_tenant ON directory_contacts(tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_contacts_team   ON directory_contacts(team)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_contacts_name   ON directory_contacts(display_name)`,

	`CREATE TABLE IF NOT EXISTS directory_addresses (
		id             TEXT PRIMARY KEY,
		contact_id     TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE,
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
		UNIQUE(kind, value)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_addr_contact ON directory_addresses(contact_id)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_addr_kind    ON directory_addresses(kind, value)`,

	`CREATE TABLE IF NOT EXISTS directory_contact_keys (
		id             TEXT PRIMARY KEY,
		contact_id     TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE,
		kind           TEXT NOT NULL,
		version        INTEGER NOT NULL DEFAULT 1,
		status         TEXT NOT NULL DEFAULT 'active',
		public_data    BLOB,
		fingerprint    TEXT NOT NULL DEFAULT '',
		algorithm      TEXT NOT NULL DEFAULT '',
		trust_anchor   TEXT NOT NULL DEFAULT 'hub',
		created_at     TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(contact_id, kind, version)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_keys_contact ON directory_contact_keys(contact_id)`,

	`CREATE TABLE IF NOT EXISTS directory_groups (
		id           TEXT PRIMARY KEY,
		tenant_id    TEXT NOT NULL DEFAULT '',
		name         TEXT NOT NULL,
		kind         TEXT NOT NULL DEFAULT 'LIST',
		sidc         TEXT NOT NULL DEFAULT '',
		mls_group_id TEXT NOT NULL DEFAULT '',
		policy_id    TEXT NOT NULL DEFAULT '',
		hub_version  INTEGER NOT NULL DEFAULT 0,
		hub_etag     TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_groups_tenant ON directory_groups(tenant_id)`,

	`CREATE TABLE IF NOT EXISTS directory_group_members (
		group_id   TEXT NOT NULL REFERENCES directory_groups(id) ON DELETE CASCADE,
		contact_id TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE,
		role       TEXT NOT NULL DEFAULT '',
		added_at   TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (group_id, contact_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_group_members_contact ON directory_group_members(contact_id)`,

	`CREATE TABLE IF NOT EXISTS directory_dispatch_policy (
		id                  TEXT PRIMARY KEY,
		tenant_id           TEXT NOT NULL DEFAULT '',
		name                TEXT NOT NULL DEFAULT '',
		scope_type          TEXT NOT NULL DEFAULT 'contact',
		scope_id            TEXT NOT NULL DEFAULT '',
		strategy            TEXT NOT NULL DEFAULT 'PRIMARY_ONLY',
		preferred           TEXT NOT NULL DEFAULT '[]',
		fallback            TEXT NOT NULL DEFAULT '[]',
		require_encryption  INTEGER NOT NULL DEFAULT 0,
		max_retries         INTEGER NOT NULL DEFAULT 0,
		retry_delay_ns      INTEGER NOT NULL DEFAULT 0,
		created_at          TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_policy_tenant ON directory_dispatch_policy(tenant_id)`,

	`CREATE TABLE IF NOT EXISTS directory_versions (
		tenant_id  TEXT PRIMARY KEY,
		version    INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
}

// NewID returns a UUIDv4 string suitable for any directory_* PK.
func NewID() string { return uuid.NewString() }

func nowUTC() string { return time.Now().UTC().Format(time.DateTime) }

// --- Contacts ----------------------------------------------------------

func (s *SQLStore) GetContact(ctx context.Context, tenantID, contactID string) (*Contact, error) {
	if tenantID == "" || contactID == "" {
		return nil, ErrEmptyID
	}
	c, err := s.loadContactRow(ctx, tenantID, contactID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if err := s.attachContactChildren(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *SQLStore) ListContacts(ctx context.Context, tenantID string) ([]Contact, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, display_name, given_name, family_name, org, role, team,
		       sidc, notes, trust_level, trust_verified_at, origin, policy_id,
		       hub_version, hub_etag, metadata, created_at, updated_at
		FROM directory_contacts WHERE tenant_id = ? ORDER BY display_name ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	for i := range out {
		if err := s.attachContactChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
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
	var cid string
	err := s.db.QueryRowContext(ctx, `
		SELECT c.id FROM directory_contacts c
		JOIN directory_addresses a ON a.contact_id = c.id
		WHERE c.tenant_id = ? AND a.kind = ? AND a.value = ?`,
		tenantID, string(kind), value).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find by address: %w", err)
	}
	return s.GetContact(ctx, tenantID, cid)
}

// PutContact inserts or updates (by primary key) a contact and its
// addresses and keys. The operation is all-or-nothing via a
// transaction — if any child insert fails the whole put rolls back.
func (s *SQLStore) PutContact(ctx context.Context, c *Contact) error {
	if c == nil {
		return fmt.Errorf("%w: nil contact", ErrEmptyID)
	}
	if c.TenantID == "" || c.DisplayName == "" {
		return fmt.Errorf("%w: tenant_id and display_name are required", ErrEmptyID)
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.Origin == "" {
		c.Origin = OriginHub
	}
	nowStr := nowUTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt, _ = time.Parse(time.DateTime, nowStr)
	}
	c.UpdatedAt, _ = time.Parse(time.DateTime, nowStr)

	metaJSON, err := encodeMetadata(c.Metadata)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var verifiedAt sql.NullString
	if c.TrustVerifiedAt != nil {
		verifiedAt.Valid = true
		verifiedAt.String = c.TrustVerifiedAt.UTC().Format(time.DateTime)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directory_contacts (
			id, tenant_id, display_name, given_name, family_name, org, role, team,
			sidc, notes, trust_level, trust_verified_at, origin, policy_id,
			hub_version, hub_etag, metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id,
			display_name = excluded.display_name,
			given_name = excluded.given_name,
			family_name = excluded.family_name,
			org = excluded.org, role = excluded.role, team = excluded.team,
			sidc = excluded.sidc, notes = excluded.notes,
			trust_level = excluded.trust_level,
			trust_verified_at = excluded.trust_verified_at,
			origin = excluded.origin, policy_id = excluded.policy_id,
			hub_version = excluded.hub_version, hub_etag = excluded.hub_etag,
			metadata = excluded.metadata, updated_at = excluded.updated_at`,
		c.ID, c.TenantID, c.DisplayName, c.GivenName, c.FamilyName, c.Org, c.Role, c.Team,
		c.SIDC, c.Notes, int(c.TrustLevel), verifiedAt, string(c.Origin), c.PolicyID,
		c.HubVersion, c.HubEtag, metaJSON, c.CreatedAt.UTC().Format(time.DateTime), c.UpdatedAt.UTC().Format(time.DateTime),
	); err != nil {
		return fmt.Errorf("put contact: %w", err)
	}

	// Replace children atomically: clear existing addresses+keys, re-insert.
	if _, err := tx.ExecContext(ctx, `DELETE FROM directory_addresses WHERE contact_id = ?`, c.ID); err != nil {
		return fmt.Errorf("reset addresses: %w", err)
	}
	for i := range c.Addresses {
		a := &c.Addresses[i]
		if a.ID == "" {
			a.ID = NewID()
		}
		if !a.Kind.Valid() {
			return fmt.Errorf("%w: %s", ErrInvalidAddressKind, a.Kind)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO directory_addresses
			(id, contact_id, kind, value, subvalue, label, primary_rank, verified, bearer_hint, max_cost_cents, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, c.ID, string(a.Kind), a.Value, a.Subvalue, a.Label,
			a.PrimaryRank, boolToInt(a.Verified), a.BearerHint, a.MaxCostCents,
			nowStr, nowStr); err != nil {
			return fmt.Errorf("put address %s: %w", a.Value, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM directory_contact_keys WHERE contact_id = ?`, c.ID); err != nil {
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
			k.CreatedAt = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO directory_contact_keys
			(id, contact_id, kind, version, status, public_data, fingerprint, algorithm, trust_anchor, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			k.ID, c.ID, k.Kind, k.Version, k.Status, k.Public, k.Fingerprint, k.Algorithm, k.TrustAnchor,
			k.CreatedAt.UTC().Format(time.DateTime)); err != nil {
			return fmt.Errorf("put key %s: %w", k.ID, err)
		}
	}
	return tx.Commit()
}

func (s *SQLStore) DeleteContact(ctx context.Context, tenantID, contactID string) error {
	if tenantID == "" || contactID == "" {
		return ErrEmptyID
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM directory_contacts WHERE tenant_id = ? AND id = ?`, tenantID, contactID)
	if err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Groups ------------------------------------------------------------

func (s *SQLStore) GetGroup(ctx context.Context, tenantID, groupID string) (*Group, error) {
	if tenantID == "" || groupID == "" {
		return nil, ErrEmptyID
	}
	g, err := s.loadGroupRow(ctx, tenantID, groupID)
	if err != nil || g == nil {
		return g, err
	}
	g.MemberIDs, err = s.groupMemberIDs(ctx, groupID)
	return g, err
}

func (s *SQLStore) ListGroups(ctx context.Context, tenantID string) ([]Group, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, kind, sidc, mls_group_id, policy_id,
		       hub_version, hub_etag, created_at, updated_at
		FROM directory_groups WHERE tenant_id = ? ORDER BY name ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	for i := range out {
		mids, err := s.groupMemberIDs(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].MemberIDs = mids
	}
	return out, nil
}

func (s *SQLStore) PutGroup(ctx context.Context, g *Group) error {
	if g == nil || g.TenantID == "" || g.Name == "" {
		return fmt.Errorf("%w: tenant_id and name required", ErrEmptyID)
	}
	if g.ID == "" {
		g.ID = NewID()
	}
	if g.Kind == "" {
		g.Kind = "LIST"
	}
	nowStr := nowUTC()
	if g.CreatedAt.IsZero() {
		g.CreatedAt, _ = time.Parse(time.DateTime, nowStr)
	}
	g.UpdatedAt, _ = time.Parse(time.DateTime, nowStr)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directory_groups (id, tenant_id, name, kind, sidc, mls_group_id, policy_id, hub_version, hub_etag, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id, name = excluded.name, kind = excluded.kind,
			sidc = excluded.sidc, mls_group_id = excluded.mls_group_id,
			policy_id = excluded.policy_id, hub_version = excluded.hub_version,
			hub_etag = excluded.hub_etag, updated_at = excluded.updated_at`,
		g.ID, g.TenantID, g.Name, g.Kind, g.SIDC, g.MLSGroupID, g.PolicyID,
		g.HubVersion, g.HubEtag, g.CreatedAt.UTC().Format(time.DateTime), g.UpdatedAt.UTC().Format(time.DateTime)); err != nil {
		return fmt.Errorf("put group: %w", err)
	}

	// Replace members atomically.
	if _, err := tx.ExecContext(ctx, `DELETE FROM directory_group_members WHERE group_id = ?`, g.ID); err != nil {
		return fmt.Errorf("reset members: %w", err)
	}
	for _, cid := range g.MemberIDs {
		if cid == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO directory_group_members (group_id, contact_id, role, added_at)
			VALUES (?, ?, '', ?)`, g.ID, cid, nowStr); err != nil {
			return fmt.Errorf("add member %s: %w", cid, err)
		}
	}
	return tx.Commit()
}

func (s *SQLStore) DeleteGroup(ctx context.Context, tenantID, groupID string) error {
	if tenantID == "" || groupID == "" {
		return ErrEmptyID
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM directory_groups WHERE tenant_id = ? AND id = ?`, tenantID, groupID)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Policies ----------------------------------------------------------

func (s *SQLStore) GetPolicy(ctx context.Context, tenantID, policyID string) (*DispatchPolicy, error) {
	if tenantID == "" || policyID == "" {
		return nil, ErrEmptyID
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, scope_type, scope_id, strategy, preferred, fallback,
		       require_encryption, max_retries, retry_delay_ns, created_at, updated_at
		FROM directory_dispatch_policy WHERE tenant_id = ? AND id = ?`, tenantID, policyID)
	p, err := scanPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *SQLStore) ListPolicies(ctx context.Context, tenantID string) ([]DispatchPolicy, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, scope_type, scope_id, strategy, preferred, fallback,
		       require_encryption, max_retries, retry_delay_ns, created_at, updated_at
		FROM directory_dispatch_policy WHERE tenant_id = ? ORDER BY scope_type ASC, scope_id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DispatchPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *SQLStore) PutPolicy(ctx context.Context, p *DispatchPolicy) error {
	if p == nil {
		return fmt.Errorf("%w: nil policy", ErrEmptyID)
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	nowStr := nowUTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt, _ = time.Parse(time.DateTime, nowStr)
	}
	p.UpdatedAt, _ = time.Parse(time.DateTime, nowStr)
	preferredJSON, _ := json.Marshal(p.Preferred)
	fallbackJSON, _ := json.Marshal(p.Fallback)
	tenantID, _ := ctxTenantID(ctx) // best-effort; policies can be cross-tenant defaults
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO directory_dispatch_policy
		(id, tenant_id, name, scope_type, scope_id, strategy, preferred, fallback,
		 require_encryption, max_retries, retry_delay_ns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tenant_id = excluded.tenant_id, name = excluded.name,
			scope_type = excluded.scope_type, scope_id = excluded.scope_id,
			strategy = excluded.strategy, preferred = excluded.preferred,
			fallback = excluded.fallback, require_encryption = excluded.require_encryption,
			max_retries = excluded.max_retries, retry_delay_ns = excluded.retry_delay_ns,
			updated_at = excluded.updated_at`,
		p.ID, tenantID, p.Name, p.ScopeType, p.ScopeID, string(p.Strategy),
		string(preferredJSON), string(fallbackJSON),
		boolToInt(p.RequireEncryption), p.MaxRetries, int64(p.RetryDelay),
		p.CreatedAt.UTC().Format(time.DateTime), p.UpdatedAt.UTC().Format(time.DateTime),
	); err != nil {
		return fmt.Errorf("put policy: %w", err)
	}
	return nil
}

func (s *SQLStore) DeletePolicy(ctx context.Context, tenantID, policyID string) error {
	if tenantID == "" || policyID == "" {
		return ErrEmptyID
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM directory_dispatch_policy WHERE tenant_id = ? AND id = ?`, tenantID, policyID)
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Versioning + snapshot --------------------------------------------

func (s *SQLStore) CurrentVersion(ctx context.Context, tenantID string) (int64, error) {
	if tenantID == "" {
		return 0, ErrEmptyID
	}
	var v int64
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM directory_versions WHERE tenant_id = ?`, tenantID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (s *SQLStore) BumpVersion(ctx context.Context, tenantID string) (int64, error) {
	if tenantID == "" {
		return 0, ErrEmptyID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	var v int64
	err = tx.QueryRowContext(ctx,
		`SELECT version FROM directory_versions WHERE tenant_id = ?`, tenantID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		v = 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO directory_versions (tenant_id, version, updated_at) VALUES (?, ?, ?)`,
			tenantID, v, nowUTC()); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	} else {
		v++
		if _, err := tx.ExecContext(ctx,
			`UPDATE directory_versions SET version = ?, updated_at = ? WHERE tenant_id = ?`,
			v, nowUTC(), tenantID); err != nil {
			return 0, err
		}
	}
	return v, tx.Commit()
}

func (s *SQLStore) Snapshot(ctx context.Context, tenantID string) (*Snapshot, error) {
	if tenantID == "" {
		return nil, ErrEmptyID
	}
	contacts, err := s.ListContacts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	groups, err := s.ListGroups(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	policies, err := s.ListPolicies(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	version, err := s.CurrentVersion(ctx, tenantID)
	if err != nil {
		return nil, err
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

func (s *SQLStore) loadContactRow(ctx context.Context, tenantID, id string) (*Contact, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, display_name, given_name, family_name, org, role, team,
		       sidc, notes, trust_level, trust_verified_at, origin, policy_id,
		       hub_version, hub_etag, metadata, created_at, updated_at
		FROM directory_contacts WHERE tenant_id = ? AND id = ?`, tenantID, id)
	c, err := scanContact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *SQLStore) attachContactChildren(ctx context.Context, c *Contact) error {
	addrs, err := s.loadAddresses(ctx, c.ID)
	if err != nil {
		return err
	}
	c.Addresses = addrs
	keys, err := s.loadKeys(ctx, c.ID)
	if err != nil {
		return err
	}
	c.Keys = keys
	gids, err := s.groupIDsForContact(ctx, c.ID)
	if err != nil {
		return err
	}
	c.GroupIDs = gids
	return nil
}

func (s *SQLStore) loadAddresses(ctx context.Context, contactID string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, value, subvalue, label, primary_rank, verified, bearer_hint, max_cost_cents
		FROM directory_addresses WHERE contact_id = ? ORDER BY primary_rank ASC, kind ASC`, contactID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Address
	for rows.Next() {
		var a Address
		var kind string
		var verified int
		var maxCost sql.NullInt64
		if err := rows.Scan(&a.ID, &kind, &a.Value, &a.Subvalue, &a.Label, &a.PrimaryRank, &verified, &a.BearerHint, &maxCost); err != nil {
			return nil, err
		}
		a.Kind = AddressKind(kind)
		a.Verified = verified != 0
		if maxCost.Valid {
			v := int(maxCost.Int64)
			a.MaxCostCents = &v
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *SQLStore) loadKeys(ctx context.Context, contactID string) ([]ContactKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, version, status, public_data, fingerprint, algorithm, trust_anchor, created_at
		FROM directory_contact_keys WHERE contact_id = ? ORDER BY kind ASC, version DESC`, contactID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ContactKey
	for rows.Next() {
		var k ContactKey
		var created sql.NullString
		if err := rows.Scan(&k.ID, &k.Kind, &k.Version, &k.Status, &k.Public, &k.Fingerprint, &k.Algorithm, &k.TrustAnchor, &created); err != nil {
			return nil, err
		}
		if created.Valid {
			k.CreatedAt, _ = time.Parse(time.DateTime, created.String)
		}
		out = append(out, k)
	}
	return out, nil
}

func (s *SQLStore) groupIDsForContact(ctx context.Context, contactID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT group_id FROM directory_group_members WHERE contact_id = ? ORDER BY added_at ASC`, contactID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *SQLStore) groupMemberIDs(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT contact_id FROM directory_group_members WHERE group_id = ? ORDER BY added_at ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *SQLStore) loadGroupRow(ctx context.Context, tenantID, id string) (*Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, kind, sidc, mls_group_id, policy_id,
		       hub_version, hub_etag, created_at, updated_at
		FROM directory_groups WHERE tenant_id = ? AND id = ?`, tenantID, id)
	g, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// rowScanner is the narrow scan contract shared by *sql.Row and
// *sql.Rows (both expose Scan(dest ...any) error).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanContact(r rowScanner) (*Contact, error) {
	var (
		c          Contact
		origin     sql.NullString
		verifiedAt sql.NullString
		metaJSON   sql.NullString
		createdAt  sql.NullString
		updatedAt  sql.NullString
		trustLevel int
	)
	err := r.Scan(&c.ID, &c.TenantID, &c.DisplayName, &c.GivenName, &c.FamilyName, &c.Org,
		&c.Role, &c.Team, &c.SIDC, &c.Notes, &trustLevel, &verifiedAt, &origin, &c.PolicyID,
		&c.HubVersion, &c.HubEtag, &metaJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	c.TrustLevel = TrustLevel(trustLevel)
	if origin.Valid {
		c.Origin = Origin(origin.String)
	}
	if verifiedAt.Valid {
		t, err := time.Parse(time.DateTime, verifiedAt.String)
		if err == nil {
			c.TrustVerifiedAt = &t
		}
	}
	if metaJSON.Valid && metaJSON.String != "" {
		_ = json.Unmarshal([]byte(metaJSON.String), &c.Metadata)
	}
	if createdAt.Valid {
		c.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
	}
	if updatedAt.Valid {
		c.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
	}
	return &c, nil
}

func scanGroup(r rowScanner) (*Group, error) {
	var (
		g         Group
		createdAt sql.NullString
		updatedAt sql.NullString
	)
	err := r.Scan(&g.ID, &g.TenantID, &g.Name, &g.Kind, &g.SIDC, &g.MLSGroupID, &g.PolicyID,
		&g.HubVersion, &g.HubEtag, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if createdAt.Valid {
		g.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
	}
	if updatedAt.Valid {
		g.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
	}
	return &g, nil
}

func scanPolicy(r rowScanner) (*DispatchPolicy, error) {
	var (
		p                 DispatchPolicy
		strategy          string
		preferredJSON     string
		fallbackJSON      string
		requireEncryption int
		retryDelayNs      int64
		createdAt         sql.NullString
		updatedAt         sql.NullString
	)
	err := r.Scan(&p.ID, &p.Name, &p.ScopeType, &p.ScopeID, &strategy, &preferredJSON, &fallbackJSON,
		&requireEncryption, &p.MaxRetries, &retryDelayNs, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.Strategy = Strategy(strategy)
	p.RequireEncryption = requireEncryption != 0
	p.RetryDelay = time.Duration(retryDelayNs)
	if preferredJSON != "" {
		_ = json.Unmarshal([]byte(preferredJSON), &p.Preferred)
	}
	if fallbackJSON != "" {
		_ = json.Unmarshal([]byte(fallbackJSON), &p.Fallback)
	}
	if createdAt.Valid {
		p.CreatedAt, _ = time.Parse(time.DateTime, createdAt.String)
	}
	if updatedAt.Valid {
		p.UpdatedAt, _ = time.Parse(time.DateTime, updatedAt.String)
	}
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ctxTenantID extracts a tenant_id hint from the context if present;
// returns the empty string otherwise. The store does not depend on
// any particular middleware so the lookup is deliberately narrow.
type tenantKey struct{}

// WithTenant attaches a tenantID to ctx so PutPolicy can populate the
// policy's tenant scope. Handlers that already know the tenant pass
// it through Put* via this key.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

func ctxTenantID(ctx context.Context) (string, bool) {
	v, _ := ctx.Value(tenantKey{}).(string)
	return v, v != ""
}

// compile-time interface check (keeps Resolver compatibility intact).
var _ Store = (*SQLStore)(nil)

// Ensure strings import stays used.
var _ = strings.HasPrefix
