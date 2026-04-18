package directory

import (
	"context"
	"errors"
	"testing"
	"time"
)

// memStore is an in-memory Store used only by tests. It can be configured
// with per-method errors to exercise resolver failure paths.
type memStore struct {
	contacts map[string]*Contact
	groups   map[string]*Group
	policies map[string]*DispatchPolicy
	byAddr   map[string]*Contact // keyed by kind|value

	errGetContact           error
	errGetGroup             error
	errGetPolicy            error
	errFindContactByAddress error
}

func newMemStore() *memStore {
	return &memStore{
		contacts: make(map[string]*Contact),
		groups:   make(map[string]*Group),
		policies: make(map[string]*DispatchPolicy),
		byAddr:   make(map[string]*Contact),
	}
}

func addrKey(kind AddressKind, value string) string { return string(kind) + "|" + value }

func (m *memStore) GetContact(_ context.Context, tenantID, id string) (*Contact, error) {
	if m.errGetContact != nil {
		return nil, m.errGetContact
	}
	c, ok := m.contacts[id]
	if !ok || c.TenantID != tenantID {
		return nil, nil
	}
	return c, nil
}

func (m *memStore) ListContacts(_ context.Context, tenantID string) ([]Contact, error) {
	out := make([]Contact, 0)
	for _, c := range m.contacts {
		if c.TenantID == tenantID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (m *memStore) GetGroup(_ context.Context, tenantID, id string) (*Group, error) {
	if m.errGetGroup != nil {
		return nil, m.errGetGroup
	}
	g, ok := m.groups[id]
	if !ok || g.TenantID != tenantID {
		return nil, nil
	}
	return g, nil
}

func (m *memStore) GetPolicy(_ context.Context, _ string, id string) (*DispatchPolicy, error) {
	if m.errGetPolicy != nil {
		return nil, m.errGetPolicy
	}
	p, ok := m.policies[id]
	if !ok {
		return nil, nil
	}
	return p, nil
}

func (m *memStore) FindContactByAddress(_ context.Context, tenantID string, kind AddressKind, value string) (*Contact, error) {
	if m.errFindContactByAddress != nil {
		return nil, m.errFindContactByAddress
	}
	c, ok := m.byAddr[addrKey(kind, value)]
	if !ok || c.TenantID != tenantID {
		return nil, nil
	}
	return c, nil
}

// The remaining methods below satisfy the extended Store interface
// introduced in MESHSAT-538 (mutation + versioning + snapshot). The
// Resolver tests in this file exercise only the read path; these are
// no-op / trivial implementations sufficient to keep the interface
// contract compile-clean.

func (m *memStore) ListGroups(_ context.Context, tenantID string) ([]Group, error) {
	out := make([]Group, 0)
	for _, g := range m.groups {
		if g.TenantID == tenantID {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (m *memStore) ListPolicies(_ context.Context, _ string) ([]DispatchPolicy, error) {
	out := make([]DispatchPolicy, 0, len(m.policies))
	for _, p := range m.policies {
		out = append(out, *p)
	}
	return out, nil
}

func (m *memStore) PutContact(_ context.Context, c *Contact) error {
	m.contacts[c.ID] = c
	for _, a := range c.Addresses {
		m.byAddr[addrKey(a.Kind, a.Value)] = c
	}
	return nil
}

func (m *memStore) DeleteContact(_ context.Context, tenantID, id string) error {
	if c, ok := m.contacts[id]; !ok || c.TenantID != tenantID {
		return ErrNotFound
	}
	delete(m.contacts, id)
	return nil
}

func (m *memStore) PutGroup(_ context.Context, g *Group) error { m.groups[g.ID] = g; return nil }
func (m *memStore) DeleteGroup(_ context.Context, tenantID, id string) error {
	if g, ok := m.groups[id]; !ok || g.TenantID != tenantID {
		return ErrNotFound
	}
	delete(m.groups, id)
	return nil
}

func (m *memStore) PutPolicy(_ context.Context, p *DispatchPolicy) error {
	m.policies[p.ID] = p
	return nil
}
func (m *memStore) DeletePolicy(_ context.Context, _, id string) error {
	if _, ok := m.policies[id]; !ok {
		return ErrNotFound
	}
	delete(m.policies, id)
	return nil
}

func (m *memStore) CurrentVersion(_ context.Context, _ string) (int64, error) { return 0, nil }
func (m *memStore) BumpVersion(_ context.Context, _ string) (int64, error)    { return 1, nil }
func (m *memStore) Snapshot(_ context.Context, tenantID string) (*Snapshot, error) {
	cs, _ := m.ListContacts(context.Background(), tenantID)
	gs, _ := m.ListGroups(context.Background(), tenantID)
	ps, _ := m.ListPolicies(context.Background(), tenantID)
	return &Snapshot{TenantID: tenantID, Version: 0, Contacts: cs, Groups: gs, Policies: ps}, nil
}

// seed builds a populated memStore used by the happy-path tests.
func seed() *memStore {
	m := newMemStore()
	now := time.Unix(1_700_000_000, 0).UTC()

	m.policies["p-ops"] = &DispatchPolicy{
		ID:         "p-ops",
		Name:       "Ops on-call",
		Preferred:  []AddressKind{KindIridiumSBD, KindSMS},
		Fallback:   []AddressKind{KindEmail},
		MaxRetries: 3,
		RetryDelay: 30 * time.Second,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.policies["p-group"] = &DispatchPolicy{
		ID: "p-group", Name: "Group policy", CreatedAt: now, UpdatedAt: now,
	}

	m.groups["g-field"] = &Group{
		ID: "g-field", TenantID: "t1", Name: "Field team",
		MemberIDs: []string{"c-alice"}, PolicyID: "p-group",
		CreatedAt: now, UpdatedAt: now,
	}
	m.groups["g-empty"] = &Group{
		ID: "g-empty", TenantID: "t1", Name: "Empty policy group",
		CreatedAt: now, UpdatedAt: now,
	}

	alice := &Contact{
		ID: "c-alice", TenantID: "t1", DisplayName: "Alice",
		Addresses: []Address{
			{Kind: KindIridiumSBD, Value: "300000000000000", Verified: true},
			{Kind: KindSMS, Value: "+15551234567"},
		},
		Keys: []ContactKey{
			{ID: "k1", Algorithm: "ed25519", Public: []byte{1, 2, 3}, CreatedAt: now},
		},
		PolicyID:  "p-ops",
		GroupIDs:  []string{"g-field"},
		Metadata:  map[string]string{"role": "lead"},
		CreatedAt: now, UpdatedAt: now,
	}
	m.contacts["c-alice"] = alice
	m.byAddr[addrKey(KindIridiumSBD, "300000000000000")] = alice
	m.byAddr[addrKey(KindSMS, "+15551234567")] = alice

	// Bob has no contact-level policy but is in a group that has one; he is
	// also in a group that the store does not have (stale reference).
	bob := &Contact{
		ID: "c-bob", TenantID: "t1", DisplayName: "Bob",
		Addresses: []Address{{Kind: KindEmail, Value: "bob@example.com"}},
		GroupIDs:  []string{"g-missing", "g-empty", "g-field"},
		CreatedAt: now, UpdatedAt: now,
	}
	m.contacts["c-bob"] = bob

	// Carol belongs to another tenant — used to prove tenant isolation.
	carol := &Contact{
		ID: "c-carol", TenantID: "t2", DisplayName: "Carol",
		Addresses: []Address{{Kind: KindSMS, Value: "+15557654321"}},
		CreatedAt: now, UpdatedAt: now,
	}
	m.contacts["c-carol"] = carol
	m.byAddr[addrKey(KindSMS, "+15557654321")] = carol

	// Dan has a policy that does not exist — Policy should end up nil.
	dan := &Contact{
		ID: "c-dan", TenantID: "t1", DisplayName: "Dan",
		PolicyID:  "p-missing",
		CreatedAt: now, UpdatedAt: now,
	}
	m.contacts["c-dan"] = dan

	return m
}

// ---- AddressKind.Valid ----

func TestAddressKindValid(t *testing.T) {
	all := []AddressKind{
		KindSMS, KindMeshtastic, KindAPRS, KindIridiumSBD, KindIridiumIMT,
		KindCellular, KindTAK, KindReticulum, KindZigbee, KindBLE,
		KindWebhook, KindEmail,
	}
	for _, k := range all {
		if !k.Valid() {
			t.Errorf("expected %q to be valid", k)
		}
	}
	for _, k := range []AddressKind{"", "TELEPATHY", "sms"} {
		if k.Valid() {
			t.Errorf("expected %q to be invalid", k)
		}
	}
}

// ---- NewResolver ----

func TestNewResolver(t *testing.T) {
	s := newMemStore()
	r := NewResolver(s)
	if r == nil || r.store != s {
		t.Fatal("NewResolver did not wire the store")
	}
}

// ---- Resolve ----

func TestResolve(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("empty tenant", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.Resolve(ctx, "", "c-alice"); !errors.Is(err, ErrEmptyID) {
			t.Fatalf("want ErrEmptyID, got %v", err)
		}
	})

	t.Run("empty contact", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.Resolve(ctx, "t1", ""); !errors.Is(err, ErrEmptyID) {
			t.Fatalf("want ErrEmptyID, got %v", err)
		}
	})

	t.Run("store error on GetContact", func(t *testing.T) {
		s := seed()
		s.errGetContact = boom
		r := NewResolver(s)
		if _, err := r.Resolve(ctx, "t1", "c-alice"); !errors.Is(err, boom) {
			t.Fatalf("want boom, got %v", err)
		}
	})

	t.Run("missing contact", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.Resolve(ctx, "t1", "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("tenant mismatch treated as missing", func(t *testing.T) {
		r := NewResolver(seed())
		// c-carol exists but belongs to t2.
		if _, err := r.Resolve(ctx, "t1", "c-carol"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("contact with own policy", func(t *testing.T) {
		r := NewResolver(seed())
		got, err := r.Resolve(ctx, "t1", "c-alice")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Contact.ID != "c-alice" {
			t.Errorf("wrong contact: %+v", got.Contact)
		}
		if len(got.Groups) != 1 || got.Groups[0].ID != "g-field" {
			t.Errorf("want g-field, got %+v", got.Groups)
		}
		if got.Policy == nil || got.Policy.ID != "p-ops" {
			t.Errorf("want policy p-ops, got %+v", got.Policy)
		}
	})

	t.Run("contact inherits policy from first group that has one", func(t *testing.T) {
		// Bob has GroupIDs {g-missing (absent), g-empty (no policy), g-field (p-group)}.
		// Expected: Groups = [g-empty, g-field], Policy = p-group.
		r := NewResolver(seed())
		got, err := r.Resolve(ctx, "t1", "c-bob")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(got.Groups) != 2 {
			t.Fatalf("want 2 groups (missing skipped), got %d: %+v", len(got.Groups), got.Groups)
		}
		if got.Groups[0].ID != "g-empty" || got.Groups[1].ID != "g-field" {
			t.Errorf("unexpected group order: %+v", got.Groups)
		}
		if got.Policy == nil || got.Policy.ID != "p-group" {
			t.Errorf("want inherited p-group, got %+v", got.Policy)
		}
	})

	t.Run("store error on GetGroup", func(t *testing.T) {
		s := seed()
		s.errGetGroup = boom
		r := NewResolver(s)
		if _, err := r.Resolve(ctx, "t1", "c-alice"); !errors.Is(err, boom) {
			t.Fatalf("want boom, got %v", err)
		}
	})

	t.Run("store error on GetPolicy", func(t *testing.T) {
		s := seed()
		s.errGetPolicy = boom
		r := NewResolver(s)
		if _, err := r.Resolve(ctx, "t1", "c-alice"); !errors.Is(err, boom) {
			t.Fatalf("want boom, got %v", err)
		}
	})

	t.Run("missing policy yields nil Policy, no error", func(t *testing.T) {
		r := NewResolver(seed())
		got, err := r.Resolve(ctx, "t1", "c-dan")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Policy != nil {
			t.Errorf("want nil policy, got %+v", got.Policy)
		}
	})

	t.Run("no policy and no groups", func(t *testing.T) {
		// Build a contact with no groups and no policy.
		s := newMemStore()
		s.contacts["c-solo"] = &Contact{ID: "c-solo", TenantID: "t1"}
		r := NewResolver(s)
		got, err := r.Resolve(ctx, "t1", "c-solo")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(got.Groups) != 0 || got.Policy != nil {
			t.Errorf("want empty groups and nil policy, got %+v", got)
		}
	})
}

// ---- FindByAddress ----

func TestFindByAddress(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("empty tenant", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.FindByAddress(ctx, "", KindSMS, "+1"); !errors.Is(err, ErrEmptyID) {
			t.Fatalf("want ErrEmptyID, got %v", err)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.FindByAddress(ctx, "t1", KindSMS, ""); !errors.Is(err, ErrEmptyID) {
			t.Fatalf("want ErrEmptyID, got %v", err)
		}
	})

	t.Run("invalid kind", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.FindByAddress(ctx, "t1", AddressKind("PIGEON"), "coo"); !errors.Is(err, ErrInvalidAddressKind) {
			t.Fatalf("want ErrInvalidAddressKind, got %v", err)
		}
	})

	t.Run("store error", func(t *testing.T) {
		s := seed()
		s.errFindContactByAddress = boom
		r := NewResolver(s)
		if _, err := r.FindByAddress(ctx, "t1", KindSMS, "+15551234567"); !errors.Is(err, boom) {
			t.Fatalf("want boom, got %v", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		r := NewResolver(seed())
		if _, err := r.FindByAddress(ctx, "t1", KindSMS, "+19999999999"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("tenant mismatch is not found", func(t *testing.T) {
		// Carol's SMS exists in the address index but belongs to t2.
		r := NewResolver(seed())
		if _, err := r.FindByAddress(ctx, "t1", KindSMS, "+15557654321"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		r := NewResolver(seed())
		got, err := r.FindByAddress(ctx, "t1", KindSMS, "+15551234567")
		if err != nil {
			t.Fatalf("FindByAddress: %v", err)
		}
		if got.ID != "c-alice" {
			t.Errorf("want alice, got %+v", got)
		}
	})
}
