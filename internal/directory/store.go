// Package directory defines the Contact/Address/Key/Group/DispatchPolicy data
// model used by the Hub dispatch engine, together with a Store interface for
// persistence and a Resolver that expands contacts to their effective groups
// and dispatch policy.
//
// The package is transport-agnostic: it only describes *who* a message is
// addressed to and *how* delivery should be attempted. Actual transport
// implementations (SMS, Meshtastic, Iridium, …) live in their own packages
// and consume the resolved records.
package directory

import (
	"context"
	"errors"
	"time"
)

// AddressKind enumerates the supported delivery transports.
type AddressKind string

const (
	KindSMS        AddressKind = "SMS"
	KindMeshtastic AddressKind = "MESHTASTIC"
	KindAPRS       AddressKind = "APRS"
	KindIridiumSBD AddressKind = "IRIDIUM_SBD"
	KindIridiumIMT AddressKind = "IRIDIUM_IMT"
	KindCellular   AddressKind = "CELLULAR"
	KindTAK        AddressKind = "TAK"
	KindReticulum  AddressKind = "RETICULUM"
	KindZigbee     AddressKind = "ZIGBEE"
	KindBLE        AddressKind = "BLE"
	KindWebhook    AddressKind = "WEBHOOK"
	KindEmail      AddressKind = "EMAIL"
	KindMQTT       AddressKind = "MQTT" // [MESHSAT-538] mirrors bridge-side v45
)

// Valid reports whether k is a recognised AddressKind.
func (k AddressKind) Valid() bool {
	switch k {
	case KindSMS, KindMeshtastic, KindAPRS, KindIridiumSBD, KindIridiumIMT,
		KindCellular, KindTAK, KindReticulum, KindZigbee, KindBLE,
		KindWebhook, KindEmail, KindMQTT:
		return true
	}
	return false
}

// Origin traces where a contact record came from. Callers use it to
// decide whether a row can be locally edited (local, imported, qr) or
// whether it is authoritatively owned by the Hub itself and should
// only be mutated through the directory REST. [MESHSAT-538]
type Origin string

const (
	OriginLocal    Origin = "local"
	OriginHub      Origin = "hub"
	OriginImported Origin = "imported"
	OriginQR       Origin = "qr"
)

// TrustLevel follows the Threema-style 0..3 ladder used on the bridge
// side (MESHSAT-535). Hub-issued contacts default to TrustAuto because
// the Hub is their authority; operators can elevate to TrustQR or
// TrustInPerson locally on the bridge.
type TrustLevel int

const (
	TrustUnknown  TrustLevel = 0
	TrustAuto     TrustLevel = 1
	TrustQR       TrustLevel = 2
	TrustInPerson TrustLevel = 3
)

// Policy dispatch strategies — mirror the bridge-side directory
// package so Hub-pushed policies can be understood verbatim by
// bridges. [MESHSAT-538]
type Strategy string

const (
	StrategyPrimaryOnly     Strategy = "PRIMARY_ONLY"
	StrategyAnyReachable    Strategy = "ANY_REACHABLE"
	StrategyOrderedFallback Strategy = "ORDERED_FALLBACK"
	StrategyHeMBBonded      Strategy = "HEMB_BONDED"
	StrategyAllBearers      Strategy = "ALL_BEARERS"
)

// Sentinel errors returned by Store implementations and the Resolver.
var (
	ErrNotFound           = errors.New("directory: not found")
	ErrInvalidAddressKind = errors.New("directory: invalid address kind")
	ErrEmptyID            = errors.New("directory: empty identifier")
)

// Address is one reachable endpoint for a Contact. The wire shape
// matches the bridge-side directory_addresses row so Hub snapshots
// drop directly into the bridge cache. [MESHSAT-538]
type Address struct {
	ID           string      `json:"id,omitempty"`
	Kind         AddressKind `json:"kind"`
	Value        string      `json:"value"`
	Subvalue     string      `json:"subvalue,omitempty"`
	Label        string      `json:"label,omitempty"`
	PrimaryRank  int         `json:"primary_rank"` // 0 = primary, 1+ = secondary
	Verified     bool        `json:"verified,omitempty"`
	BearerHint   int         `json:"bearer_hint,omitempty"`
	MaxCostCents *int        `json:"max_cost_cents,omitempty"`
}

// ContactKey is a key record bound to a contact for signing or E2E
// encryption. The Hub is authoritative for the public half; private
// halves (long-term X25519, AES traffic keys) live either with the
// bridge that will use them or in the per-contact keystore. Kind
// values mirror the bridge-side enum. [MESHSAT-538]
type ContactKey struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // AES256_GCM_SHARED / X25519_LT_PUB / …
	Version     int       `json:"version"`
	Status      string    `json:"status"` // active / retired / revoked
	Public      []byte    `json:"public,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	TrustAnchor string    `json:"trust_anchor,omitempty"` // hub / qr / manual / in_person / local
	Algorithm   string    `json:"algorithm,omitempty"`    // free-text; e.g. Ed25519, X25519
	CreatedAt   time.Time `json:"created_at"`
}

// DispatchPolicy describes how the Hub should attempt to reach a contact:
// which transports to try first, fallbacks, retry behaviour, and whether
// encryption is mandatory. [MESHSAT-538] The Strategy field is
// populated when a bridge-side policy row backs this record; legacy
// Hub-only policies continue to rely on Preferred/Fallback.
type DispatchPolicy struct {
	ID                string        `json:"id"`
	Name              string        `json:"name,omitempty"`
	ScopeType         string        `json:"scope_type,omitempty"` // contact / group / precedence / default
	ScopeID           string        `json:"scope_id,omitempty"`
	Strategy          Strategy      `json:"strategy,omitempty"`
	Preferred         []AddressKind `json:"preferred,omitempty"`
	Fallback          []AddressKind `json:"fallback,omitempty"`
	RequireEncryption bool          `json:"require_encryption,omitempty"`
	MaxRetries        int           `json:"max_retries,omitempty"`
	RetryDelay        time.Duration `json:"retry_delay,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// Contact is an addressable entity (person, device, bot) with one or
// more transport addresses, optional public keys, an optional dispatch
// policy, and optional group memberships. Field set mirrors
// directory_contacts in the bridge v44 schema so Hub snapshots drop
// directly into the bridge cache. [MESHSAT-538]
type Contact struct {
	ID              string            `json:"id"`
	TenantID        string            `json:"tenant_id"`
	DisplayName     string            `json:"display_name"`
	GivenName       string            `json:"given_name,omitempty"`
	FamilyName      string            `json:"family_name,omitempty"`
	Org             string            `json:"org,omitempty"`
	Role            string            `json:"role,omitempty"`
	Team            string            `json:"team,omitempty"`
	SIDC            string            `json:"sidc,omitempty"`
	Notes           string            `json:"notes,omitempty"`
	TrustLevel      TrustLevel        `json:"trust_level,omitempty"`
	TrustVerifiedAt *time.Time        `json:"trust_verified_at,omitempty"`
	Origin          Origin            `json:"origin,omitempty"`
	HubVersion      int64             `json:"hub_version,omitempty"`
	HubEtag         string            `json:"hub_etag,omitempty"`
	Addresses       []Address         `json:"addresses"`
	Keys            []ContactKey      `json:"keys,omitempty"`
	PolicyID        string            `json:"policy_id,omitempty"`
	GroupIDs        []string          `json:"group_ids,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Group is a named set of contact IDs sharing a dispatch policy. A
// contact may belong to multiple groups; group membership order is
// significant when inheriting a dispatch policy (first group with a
// policy wins). [MESHSAT-538]
type Group struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind,omitempty"` // TEAM / ROLE / LIST / MLS_GROUP (bridge-side shape)
	SIDC       string    `json:"sidc,omitempty"`
	MLSGroupID string    `json:"mls_group_id,omitempty"`
	HubVersion int64     `json:"hub_version,omitempty"`
	HubEtag    string    `json:"hub_etag,omitempty"`
	MemberIDs  []string  `json:"member_ids"`
	PolicyID   string    `json:"policy_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Snapshot is the canonical Hub → Bridge directory payload. A fresh
// snapshot is emitted on every contact/group/policy mutation per
// tenant and signed with the Hub's ECDSA-P256 directory-signing key
// (see internal/directory/signer.go). Bridges verify the signature
// against the trust anchor they received in the provisioning bundle
// (MESHSAT-539) before applying. [MESHSAT-538 / MESHSAT-540]
type Snapshot struct {
	TenantID  string           `json:"tenant_id"`
	Version   int64            `json:"version"`
	Etag      string           `json:"etag,omitempty"`
	SignedAt  time.Time        `json:"signed_at"`
	Signature []byte           `json:"signature,omitempty"` // ECDSA-P256-SHA256 over canonical(payload)
	Contacts  []Contact        `json:"contacts"`
	Groups    []Group          `json:"groups,omitempty"`
	Policies  []DispatchPolicy `json:"policies,omitempty"`
}

// Resolved is the fully-expanded view of a contact: the contact itself, any
// groups it belongs to that exist in the store, and the effective dispatch
// policy (contact-level if set, otherwise the first group policy found).
type Resolved struct {
	Contact Contact
	Groups  []Group
	Policy  *DispatchPolicy
}

// Store is the persistence contract used by the Resolver and the
// Hub REST handlers. Implementations must be tenant-scoped: every
// lookup takes a tenantID and must not return records from other
// tenants. A missing record is signalled by returning (nil, nil) for
// lookups, ErrNotFound for mutations. Transient failures should
// return a non-nil error.
//
// The mutation methods (Put*, Delete*, BumpVersion) were added in
// MESHSAT-538 to support Hub REST writes and directory_push snapshot
// assembly. They preserve the read-only contract of earlier callers —
// the Resolver still uses only the read methods.
type Store interface {
	// Reads.
	GetContact(ctx context.Context, tenantID, contactID string) (*Contact, error)
	ListContacts(ctx context.Context, tenantID string) ([]Contact, error)
	GetGroup(ctx context.Context, tenantID, groupID string) (*Group, error)
	ListGroups(ctx context.Context, tenantID string) ([]Group, error)
	GetPolicy(ctx context.Context, tenantID, policyID string) (*DispatchPolicy, error)
	ListPolicies(ctx context.Context, tenantID string) ([]DispatchPolicy, error)
	FindContactByAddress(ctx context.Context, tenantID string, kind AddressKind, value string) (*Contact, error)

	// Mutations. PutContact / PutGroup / PutPolicy insert-or-update
	// by ID; the caller is responsible for generating or preserving
	// the UUID on creation.
	PutContact(ctx context.Context, c *Contact) error
	DeleteContact(ctx context.Context, tenantID, contactID string) error
	PutGroup(ctx context.Context, g *Group) error
	DeleteGroup(ctx context.Context, tenantID, groupID string) error
	PutPolicy(ctx context.Context, p *DispatchPolicy) error
	DeletePolicy(ctx context.Context, tenantID, policyID string) error

	// Versioning. Snapshot assembles the full tenant view and bumps
	// the tenant's monotonic directory version. CurrentVersion returns
	// the latest without mutating.
	CurrentVersion(ctx context.Context, tenantID string) (int64, error)
	BumpVersion(ctx context.Context, tenantID string) (int64, error)
	Snapshot(ctx context.Context, tenantID string) (*Snapshot, error)
}

// Resolver expands Contact records into Resolved views and provides
// address-based contact lookups. It is safe for concurrent use as long as
// the underlying Store is.
type Resolver struct {
	store Store
}

// NewResolver returns a Resolver backed by s.
func NewResolver(s Store) *Resolver {
	return &Resolver{store: s}
}

// Resolve returns the Contact identified by contactID along with its groups
// and its effective DispatchPolicy.
//
// Policy resolution order:
//  1. Contact.PolicyID, if set.
//  2. Otherwise, the PolicyID of the first group (in Contact.GroupIDs order)
//     that exists and carries a policy.
//
// Missing groups are silently skipped so stale group references do not fail
// the whole resolution. A missing policy yields Resolved.Policy == nil without
// an error; the dispatcher decides what to do in that case.
func (r *Resolver) Resolve(ctx context.Context, tenantID, contactID string) (*Resolved, error) {
	if tenantID == "" || contactID == "" {
		return nil, ErrEmptyID
	}

	c, err := r.store.GetContact(ctx, tenantID, contactID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}

	res := &Resolved{Contact: *c}

	for _, gid := range c.GroupIDs {
		g, err := r.store.GetGroup(ctx, tenantID, gid)
		if err != nil {
			return nil, err
		}
		if g == nil {
			continue
		}
		res.Groups = append(res.Groups, *g)
	}

	policyID := c.PolicyID
	if policyID == "" {
		for _, g := range res.Groups {
			if g.PolicyID != "" {
				policyID = g.PolicyID
				break
			}
		}
	}
	if policyID != "" {
		p, err := r.store.GetPolicy(ctx, tenantID, policyID)
		if err != nil {
			return nil, err
		}
		res.Policy = p
	}

	return res, nil
}

// FindByAddress returns the contact that owns the address (kind, value) pair
// within tenantID. Returns ErrInvalidAddressKind for unknown kinds, ErrEmptyID
// for empty tenant or value, and ErrNotFound if no contact matches.
func (r *Resolver) FindByAddress(ctx context.Context, tenantID string, kind AddressKind, value string) (*Contact, error) {
	if tenantID == "" || value == "" {
		return nil, ErrEmptyID
	}
	if !kind.Valid() {
		return nil, ErrInvalidAddressKind
	}
	c, err := r.store.FindContactByAddress(ctx, tenantID, kind, value)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	return c, nil
}
