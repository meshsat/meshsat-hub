// Package directorytest is the directory store's contract, run against
// every driver: SQLite in internal/directory and Postgres in
// internal/store/postgres (the CI test:postgres job runs that package).
// [MESHSAT-1584 C7]
//
// The victim tenant in the isolation cases is store.DefaultTenantID on
// purpose: a store that lost its tenant dimension collapses onto the
// default tenant, and a test between two other tenants passes by accident
// (.claude/rules/tenancy.md).
package directorytest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Opener returns a store on an empty, migrated database.
type Opener func(t *testing.T) *directory.SQLStore

const (
	victim   = store.DefaultTenantID
	attacker = "t-other"
)

// Run runs every case against stores from open.
func Run(t *testing.T, open Opener) {
	cases := []struct {
		name string
		fn   func(t *testing.T, s *directory.SQLStore)
	}{
		{"ContactRoundTrip", contactRoundTrip},
		{"TenantIsolation", tenantIsolation},
		{"AddressBelongsToOneContact", addressBelongsToOneContact},
		{"EveryMutationBumpsOnceAndAFailureNever", versionPerMutation},
		{"SnapshotVersionDescribesItsRows", snapshotVersionDescribesItsRows},
		{"SnapshotIsDeterministic", snapshotIsDeterministic},
		{"GroupMembers", groupMembers},
		{"Policies", policies},
		{"SeedIsOncePerScopeUnderConcurrency", seedConcurrent},
		{"Guards", guards},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

func version(t *testing.T, s *directory.SQLStore, tenant string) int64 {
	t.Helper()
	v, err := s.CurrentVersion(context.Background(), tenant)
	if err != nil {
		t.Fatalf("CurrentVersion(%s): %v", tenant, err)
	}
	return v
}

func mustPut(t *testing.T, s *directory.SQLStore, c *directory.Contact) {
	t.Helper()
	if err := s.PutContact(context.Background(), c); err != nil {
		t.Fatalf("PutContact %s: %v", c.DisplayName, err)
	}
}

func contactRoundTrip(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	verified := time.Date(2026, 10, 20, 9, 30, 15, 0, time.UTC)
	cost := 25
	c := &directory.Contact{
		TenantID: victim, DisplayName: "Alice Kowalski", GivenName: "Alice", FamilyName: "Kowalski",
		Org: "Red Cross", Role: "Medic", Team: "Red", SIDC: "SFGPU", Notes: "n",
		TrustLevel: directory.TrustQR, TrustVerifiedAt: &verified, Origin: directory.OriginImported,
		PolicyID: "p-1", HubVersion: 7, HubEtag: "e7",
		Metadata: map[string]string{"callsign": "ALPHA"},
		Addresses: []directory.Address{
			{Kind: directory.KindMeshtastic, Value: "!abcd1234", PrimaryRank: 1, Verified: true, BearerHint: 60},
			{Kind: directory.KindSMS, Value: "+31612345678", Label: "Mobile", PrimaryRank: 0, MaxCostCents: &cost},
		},
		Keys: []directory.ContactKey{{Kind: "X25519_LT_PUB", Public: []byte{1, 2, 3}, Fingerprint: "fp", Algorithm: "X25519", TrustAnchor: "hub"}},
	}
	mustPut(t, s, c)
	if c.ID == "" || c.CreatedAt.IsZero() {
		t.Fatalf("put did not stamp id/created_at: %+v", c)
	}
	got, err := s.GetContact(ctx, victim, c.ID)
	if err != nil || got == nil {
		t.Fatalf("GetContact: %+v %v", got, err)
	}
	if got.DisplayName != c.DisplayName || got.GivenName != "Alice" || got.Org != "Red Cross" || got.SIDC != "SFGPU" ||
		got.TrustLevel != directory.TrustQR || got.Origin != directory.OriginImported || got.PolicyID != "p-1" ||
		got.HubVersion != 7 || got.HubEtag != "e7" || got.Metadata["callsign"] != "ALPHA" {
		t.Errorf("fields did not round-trip: %+v", got)
	}
	if got.TrustVerifiedAt == nil || !got.TrustVerifiedAt.Equal(verified) {
		t.Errorf("trust_verified_at: %v, want %v", got.TrustVerifiedAt, verified)
	}
	if len(got.Addresses) != 2 || got.Addresses[0].Kind != directory.KindSMS || got.Addresses[1].Kind != directory.KindMeshtastic {
		t.Fatalf("addresses not in rank order: %+v", got.Addresses)
	}
	if a := got.Addresses[0]; a.MaxCostCents == nil || *a.MaxCostCents != 25 || a.Label != "Mobile" || a.Verified {
		t.Errorf("sms address: %+v", a)
	}
	if a := got.Addresses[1]; !a.Verified || a.BearerHint != 60 || a.MaxCostCents != nil {
		t.Errorf("mesh address: %+v", a)
	}
	if len(got.Keys) != 1 || !bytes.Equal(got.Keys[0].Public, []byte{1, 2, 3}) || got.Keys[0].Version != 1 ||
		got.Keys[0].Status != "active" || got.Keys[0].Fingerprint != "fp" || got.Keys[0].CreatedAt.IsZero() {
		t.Errorf("keys: %+v", got.Keys)
	}
	created := got.CreatedAt

	// An update by id: created_at stays the stored one even when the
	// caller sends none; the children are replaced, not merged.
	up := &directory.Contact{ID: c.ID, TenantID: victim, DisplayName: "Alice K.",
		Addresses: []directory.Address{{Kind: directory.KindAPRS, Value: "PD0ABC-7"}}}
	mustPut(t, s, up)
	if !up.CreatedAt.Equal(created) {
		t.Errorf("update returned created_at %v, want the stored %v", up.CreatedAt, created)
	}
	got, _ = s.GetContact(ctx, victim, c.ID)
	if got.DisplayName != "Alice K." || len(got.Addresses) != 1 || got.Addresses[0].Kind != directory.KindAPRS ||
		len(got.Keys) != 0 || !got.CreatedAt.Equal(created) {
		t.Errorf("update: %+v", got)
	}
	if old, _ := s.FindContactByAddress(ctx, victim, directory.KindSMS, "+31612345678"); old != nil {
		t.Errorf("replaced address still resolves to %s", old.ID)
	}
}

func tenantIsolation(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	v := &directory.Contact{ID: "shared-id", TenantID: victim, DisplayName: "Victim",
		Addresses: []directory.Address{{ID: "addr-shared", Kind: directory.KindSMS, Value: "+31600000001"}}}
	mustPut(t, s, v)
	only := &directory.Contact{ID: "victim-only", TenantID: victim, DisplayName: "Victim 2"}
	mustPut(t, s, only)
	vg := &directory.Group{ID: "group-shared", TenantID: victim, Name: "Victims", MemberIDs: []string{"shared-id"}}
	if err := s.PutGroup(ctx, vg); err != nil {
		t.Fatal(err)
	}
	vp := &directory.DispatchPolicy{ID: "policy-shared", TenantID: victim, ScopeType: "contact", ScopeID: "shared-id",
		Strategy: directory.StrategyAnyReachable}
	if err := s.PutPolicy(ctx, vp); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, &directory.Contact{ID: "v-dup", TenantID: victim, DisplayName: "V dup",
		Addresses: []directory.Address{{Kind: directory.KindEmail, Value: "dup@example.org"}}})
	before := version(t, s, victim)

	// The same ids and the same address in another tenant are that
	// tenant's own rows; nothing of the victim's moves.
	a := &directory.Contact{ID: "shared-id", TenantID: attacker, DisplayName: "Attacker",
		Addresses: []directory.Address{{ID: "addr-shared", Kind: directory.KindSMS, Value: "+31600000001"}}}
	mustPut(t, s, a)
	if err := s.PutGroup(ctx, &directory.Group{ID: "group-shared", TenantID: attacker, Name: "Mine",
		MemberIDs: []string{"shared-id"}}); err != nil {
		t.Fatalf("attacker's own group: %v", err)
	}
	if err := s.PutPolicy(ctx, &directory.DispatchPolicy{ID: "policy-shared", TenantID: attacker, ScopeType: "contact",
		ScopeID: "shared-id", Strategy: directory.StrategyPrimaryOnly}); err != nil {
		t.Fatalf("attacker's own policy: %v", err)
	}
	// A victim's contact is not a member another tenant can name.
	if err := s.PutGroup(ctx, &directory.Group{TenantID: attacker, Name: "Steal", MemberIDs: []string{"victim-only"}}); !errors.Is(err, directory.ErrUnknownMember) {
		t.Errorf("group naming another tenant's contact: err=%v, want ErrUnknownMember", err)
	}
	for _, del := range []struct {
		name string
		err  error
	}{
		{"contact", s.DeleteContact(ctx, attacker, "victim-only")},
		{"group", s.DeleteGroup(ctx, attacker, "no-such-group")},
		{"policy", s.DeletePolicy(ctx, attacker, "no-such-policy")},
	} {
		if !errors.Is(del.err, directory.ErrNotFound) {
			t.Errorf("attacker deleting a victim-only %s: err=%v, want ErrNotFound", del.name, del.err)
		}
	}
	if err := s.DeleteContact(ctx, attacker, "shared-id"); err != nil {
		t.Fatalf("attacker deleting its own shared-id: %v", err)
	}

	got, _ := s.GetContact(ctx, victim, "shared-id")
	if got == nil || got.DisplayName != "Victim" || len(got.Addresses) != 1 || got.Addresses[0].ID != "addr-shared" ||
		len(got.GroupIDs) != 1 || got.GroupIDs[0] != "group-shared" {
		t.Fatalf("victim contact changed by another tenant: %+v", got)
	}
	if c, _ := s.FindContactByAddress(ctx, victim, directory.KindSMS, "+31600000001"); c == nil || c.TenantID != victim {
		t.Errorf("victim address resolves to %+v", c)
	}
	if c, _ := s.GetContact(ctx, victim, "victim-only"); c == nil {
		t.Error("victim-only contact gone")
	}
	// One address held in both tenants by contacts with different ids:
	// each tenant's lookup finds its own (a lookup that ignored the
	// tenant would hand one of them the other's row).
	mustPut(t, s, &directory.Contact{ID: "a-dup", TenantID: attacker, DisplayName: "A dup",
		Addresses: []directory.Address{{Kind: directory.KindEmail, Value: "dup@example.org"}}})
	for tenant, want := range map[string]string{victim: "v-dup", attacker: "a-dup"} {
		if c, err := s.FindContactByAddress(ctx, tenant, directory.KindEmail, "dup@example.org"); err != nil || c == nil || c.ID != want {
			t.Errorf("%s looking up an address both tenants hold: %+v %v, want %s", tenant, c, err, want)
		}
	}
	g, _ := s.GetGroup(ctx, victim, "group-shared")
	if g == nil || g.Name != "Victims" || len(g.MemberIDs) != 1 {
		t.Errorf("victim group changed: %+v", g)
	}
	p, _ := s.GetPolicy(ctx, victim, "policy-shared")
	if p == nil || p.Strategy != directory.StrategyAnyReachable || p.TenantID != victim {
		t.Errorf("victim policy changed: %+v", p)
	}
	if after := version(t, s, victim); after != before {
		t.Errorf("another tenant's writes moved the victim's version %d -> %d", before, after)
	}
	snap, err := s.Snapshot(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Contacts {
		if c.TenantID != victim || c.DisplayName == "Attacker" {
			t.Errorf("victim snapshot carries %+v", c)
		}
	}
	for _, g := range snap.Groups {
		if g.TenantID != victim || g.Name == "Mine" {
			t.Errorf("victim snapshot carries group %+v", g)
		}
	}
	for _, p := range snap.Policies {
		if p.TenantID != victim || p.Strategy == directory.StrategyPrimaryOnly {
			t.Errorf("victim snapshot carries policy %+v", p)
		}
	}
	if len(snap.Contacts) != 3 || len(snap.Groups) != 1 || len(snap.Policies) != 1 {
		t.Errorf("victim snapshot sizes: %d contacts, %d groups, %d policies", len(snap.Contacts), len(snap.Groups), len(snap.Policies))
	}
}

func addressBelongsToOneContact(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	c1 := &directory.Contact{TenantID: victim, DisplayName: "One",
		Addresses: []directory.Address{{ID: "a-1", Kind: directory.KindSMS, Value: "+31611111111"}},
		Keys:      []directory.ContactKey{{ID: "k-1", Kind: "X25519_LT_PUB"}}}
	mustPut(t, s, c1)
	v := version(t, s, victim)

	for _, tc := range []struct {
		name string
		c    *directory.Contact
		want error
	}{
		{"same kind and value", &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
			Addresses: []directory.Address{{Kind: directory.KindSMS, Value: "+31611111111"}}}, directory.ErrAddressTaken},
		{"same address id", &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
			Addresses: []directory.Address{{ID: "a-1", Kind: directory.KindSMS, Value: "+31622222222"}}}, directory.ErrAddressTaken},
		{"one address twice", &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
			Addresses: []directory.Address{{Kind: directory.KindEmail, Value: "x@example.org"}, {Kind: directory.KindEmail, Value: "x@example.org"}}},
			directory.ErrAddressTaken},
		{"same key id", &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
			Keys: []directory.ContactKey{{ID: "k-1", Kind: "X25519_LT_PUB"}}}, directory.ErrKeyTaken},
		{"invalid kind", &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
			Addresses: []directory.Address{{Kind: "BOGUS", Value: "x"}}}, directory.ErrInvalidAddressKind},
	} {
		if err := s.PutContact(ctx, tc.c); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, want %v", tc.name, err, tc.want)
		}
		if got, _ := s.GetContact(ctx, victim, "two"); got != nil {
			t.Errorf("%s: the failed put left contact %+v", tc.name, got)
		}
		if after := version(t, s, victim); after != v {
			t.Errorf("%s: a failed put moved the version %d -> %d", tc.name, v, after)
		}
	}
	// The owner may put its own address again.
	mustPut(t, s, c1)
	if got := version(t, s, victim); got != v+1 {
		t.Errorf("re-put of the owner: version %d, want %d", got, v+1)
	}
	// Once the owner is gone the address, its id and the key id are free.
	if err := s.DeleteContact(ctx, victim, c1.ID); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, &directory.Contact{ID: "two", TenantID: victim, DisplayName: "Two",
		Addresses: []directory.Address{{ID: "a-1", Kind: directory.KindSMS, Value: "+31611111111"}},
		Keys:      []directory.ContactKey{{ID: "k-1", Kind: "X25519_LT_PUB"}}})
	if c, _ := s.FindContactByAddress(ctx, victim, directory.KindSMS, "+31611111111"); c == nil || c.ID != "two" {
		t.Errorf("freed address resolves to %+v", c)
	}
}

func versionPerMutation(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	if v := version(t, s, victim); v != 0 {
		t.Fatalf("fresh tenant at version %d", v)
	}
	c := &directory.Contact{TenantID: victim, DisplayName: "C"}
	g := &directory.Group{TenantID: victim, Name: "G"}
	p := &directory.DispatchPolicy{TenantID: victim, ScopeType: "group", ScopeID: "G", Strategy: directory.StrategyAllBearers}
	steps := []struct {
		name string
		do   func() error
	}{
		{"put contact", func() error { return s.PutContact(ctx, c) }},
		{"update contact", func() error { return s.PutContact(ctx, c) }},
		{"put group", func() error { g.MemberIDs = []string{c.ID}; return s.PutGroup(ctx, g) }},
		{"put policy", func() error { return s.PutPolicy(ctx, p) }},
		{"delete policy", func() error { return s.DeletePolicy(ctx, victim, p.ID) }},
		{"delete group", func() error { return s.DeleteGroup(ctx, victim, g.ID) }},
		{"delete contact", func() error { return s.DeleteContact(ctx, victim, c.ID) }},
	}
	for i, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if v := version(t, s, victim); v != int64(i+1) {
			t.Errorf("after %s: version %d, want %d", st.name, v, i+1)
		}
	}
	n := int64(len(steps))
	failures := []struct {
		name string
		err  error
	}{
		{"delete missing contact", s.DeleteContact(ctx, victim, "missing")},
		{"delete missing group", s.DeleteGroup(ctx, victim, "missing")},
		{"delete missing policy", s.DeletePolicy(ctx, victim, "missing")},
		{"group with unknown member", s.PutGroup(ctx, &directory.Group{TenantID: victim, Name: "X", MemberIDs: []string{"missing"}})},
		{"policy without tenant", s.PutPolicy(ctx, &directory.DispatchPolicy{ScopeType: "default"})},
	}
	for _, f := range failures {
		if f.err == nil {
			t.Errorf("%s: no error", f.name)
		}
		if v := version(t, s, victim); v != n {
			t.Errorf("%s moved the version to %d, want %d", f.name, v, n)
		}
	}
	inserted, err := s.SeedPrecedenceDefaults(ctx, victim)
	if err != nil || inserted != len(directory.PrecedenceDefaults) {
		t.Fatalf("seed: %d %v", inserted, err)
	}
	if v := version(t, s, victim); v != n+1 {
		t.Errorf("seed: version %d, want %d (one bump for the whole seed)", v, n+1)
	}
	if again, err := s.SeedPrecedenceDefaults(ctx, victim); err != nil || again != 0 {
		t.Errorf("re-seed: %d %v", again, err)
	}
	if v := version(t, s, victim); v != n+1 {
		t.Errorf("a seed that inserted nothing moved the version to %d", v)
	}
	if v := version(t, s, attacker); v != 0 {
		t.Errorf("another tenant's version is %d", v)
	}
}

// snapshotVersionDescribesItsRows: every put adds one contact and bumps
// once, so a snapshot taken at any moment, however the writers interleave,
// carries exactly as many contacts as its version says.
func snapshotVersionDescribesItsRows(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	const writers, each = 4, 15
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c := &directory.Contact{TenantID: victim, DisplayName: fmt.Sprintf("w%d-%02d", w, i),
					Addresses: []directory.Address{{Kind: directory.KindSMS, Value: fmt.Sprintf("+3160%d%04d", w, i)}}}
				if err := s.PutContact(ctx, c); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	snaps := 0
	for {
		snap, err := s.Snapshot(ctx, victim)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		snaps++
		if int64(len(snap.Contacts)) != snap.Version {
			t.Fatalf("snapshot at version %d carries %d contacts", snap.Version, len(snap.Contacts))
		}
		select {
		case <-done:
		default:
			continue
		}
		break
	}
	close(errs)
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}
	final, err := s.Snapshot(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if final.Version != writers*each || len(final.Contacts) != writers*each {
		t.Errorf("final snapshot: version %d, %d contacts, want %d", final.Version, len(final.Contacts), writers*each)
	}
	if cur := version(t, s, victim); cur != final.Version {
		t.Errorf("CurrentVersion %d, snapshot %d", cur, final.Version)
	}
	t.Logf("%d snapshots taken during the writes", snaps)
}

// snapshotIsDeterministic: with nothing changed two snapshots are the same
// bytes but for the signing time, ties included (one display name, one
// rank and kind), because the signature covers the bytes.
func snapshotIsDeterministic(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	for _, id := range []string{"c-b", "c-a", "c-c"} {
		mustPut(t, s, &directory.Contact{ID: id, TenantID: victim, DisplayName: "Same Name",
			Addresses: []directory.Address{
				{Kind: directory.KindEmail, Value: id + "-2@example.org"},
				{Kind: directory.KindEmail, Value: id + "-1@example.org"},
			}})
	}
	if err := s.PutGroup(ctx, &directory.Group{TenantID: victim, Name: "Team", Kind: "TEAM", MemberIDs: []string{"c-c", "c-a", "c-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedPrecedenceDefaults(ctx, victim); err != nil {
		t.Fatal(err)
	}
	canon := func() []byte {
		snap, err := s.Snapshot(ctx, victim)
		if err != nil {
			t.Fatal(err)
		}
		snap.SignedAt = time.Time{}
		b, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := canon()
	for i := 0; i < 5; i++ {
		if again := canon(); !bytes.Equal(first, again) {
			t.Fatalf("snapshot bytes changed with no write:\n%s\n%s", first, again)
		}
	}
	snap, _ := s.Snapshot(ctx, victim)
	if snap.Contacts[0].ID != "c-a" || snap.Contacts[1].ID != "c-b" || snap.Contacts[2].ID != "c-c" {
		t.Errorf("contacts with one name are not ordered by id: %s %s %s", snap.Contacts[0].ID, snap.Contacts[1].ID, snap.Contacts[2].ID)
	}
	if a := snap.Contacts[0].Addresses; a[0].Value != "c-a-1@example.org" {
		t.Errorf("addresses of one rank and kind are not ordered by value: %+v", a)
	}
}

func groupMembers(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		mustPut(t, s, &directory.Contact{ID: id, TenantID: victim, DisplayName: id})
	}
	// The given order is kept and a repeated id counts once.
	g1 := &directory.Group{ID: "zz-first", TenantID: victim, Name: "First", Kind: "TEAM", MemberIDs: []string{"c", "a", "b", "a", ""}}
	if err := s.PutGroup(ctx, g1); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(g1.MemberIDs) != "[c a b]" {
		t.Errorf("put returned members %v", g1.MemberIDs)
	}
	got, _ := s.GetGroup(ctx, victim, "zz-first")
	if got == nil || fmt.Sprint(got.MemberIDs) != "[c a b]" || got.Kind != "TEAM" {
		t.Fatalf("GetGroup: %+v", got)
	}
	list, _ := s.ListGroups(ctx, victim)
	if len(list) != 1 || fmt.Sprint(list[0].MemberIDs) != "[c a b]" {
		t.Errorf("ListGroups: %+v", list)
	}
	created := got.CreatedAt

	// b joins a second group a second later; re-putting the first group
	// keeps the time b joined it, so b's groups stay in joining order
	// (and not in id order, which would put "aa-second" first).
	time.Sleep(1100 * time.Millisecond)
	if err := s.PutGroup(ctx, &directory.Group{ID: "aa-second", TenantID: victim, Name: "Second", MemberIDs: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	g1.MemberIDs = []string{"b", "c"}
	g1.CreatedAt = time.Time{}
	if err := s.PutGroup(ctx, g1); err != nil {
		t.Fatal(err)
	}
	if !g1.CreatedAt.Equal(created) {
		t.Errorf("re-put changed created_at %v -> %v", created, g1.CreatedAt)
	}
	b, _ := s.GetContact(ctx, victim, "b")
	if fmt.Sprint(b.GroupIDs) != "[zz-first aa-second]" {
		t.Errorf("b's groups %v, want [zz-first aa-second] (the order b joined them)", b.GroupIDs)
	}
	a, _ := s.GetContact(ctx, victim, "a")
	if len(a.GroupIDs) != 0 {
		t.Errorf("a left the group but still lists %v", a.GroupIDs)
	}

	// An unknown member fails the put and leaves the group as it was.
	v := version(t, s, victim)
	bad := &directory.Group{ID: "zz-first", TenantID: victim, Name: "Renamed", MemberIDs: []string{"a", "nobody"}}
	if err := s.PutGroup(ctx, bad); !errors.Is(err, directory.ErrUnknownMember) {
		t.Fatalf("unknown member: err=%v", err)
	}
	got, _ = s.GetGroup(ctx, victim, "zz-first")
	if got.Name != "First" || fmt.Sprint(got.MemberIDs) != "[b c]" || version(t, s, victim) != v {
		t.Errorf("failed put changed the group: %+v (version %d -> %d)", got, v, version(t, s, victim))
	}

	// A deleted contact leaves every group; a deleted group leaves every contact.
	if err := s.DeleteContact(ctx, victim, "c"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGroup(ctx, victim, "zz-first")
	if fmt.Sprint(got.MemberIDs) != "[b]" {
		t.Errorf("deleted contact still a member: %v", got.MemberIDs)
	}
	if err := s.DeleteGroup(ctx, victim, "aa-second"); err != nil {
		t.Fatal(err)
	}
	b, _ = s.GetContact(ctx, victim, "b")
	if fmt.Sprint(b.GroupIDs) != "[zz-first]" {
		t.Errorf("deleted group still listed: %v", b.GroupIDs)
	}
	if g, _ := s.GetGroup(ctx, victim, "aa-second"); g != nil {
		t.Errorf("deleted group still readable: %+v", g)
	}
}

func policies(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	p1 := &directory.DispatchPolicy{TenantID: victim, Name: "Flash", ScopeType: "contact", ScopeID: "x",
		Strategy: directory.StrategyHeMBBonded, Preferred: []directory.AddressKind{directory.KindMeshtastic, directory.KindSMS},
		Fallback: []directory.AddressKind{directory.KindIridiumIMT}, RequireEncryption: true, MaxRetries: 3, RetryDelay: 2 * time.Second}
	if err := s.PutPolicy(ctx, p1); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPolicy(ctx, victim, p1.ID)
	if got == nil || got.Strategy != directory.StrategyHeMBBonded || len(got.Preferred) != 2 || got.Fallback[0] != directory.KindIridiumIMT ||
		!got.RequireEncryption || got.MaxRetries != 3 || got.RetryDelay != 2*time.Second || got.Seeded || got.TenantID != victim {
		t.Errorf("policy round-trip: %+v", got)
	}
	// One policy per (tenant, scope).
	dup := &directory.DispatchPolicy{TenantID: victim, ScopeType: "contact", ScopeID: "x", Strategy: directory.StrategyPrimaryOnly}
	if err := s.PutPolicy(ctx, dup); !errors.Is(err, directory.ErrPolicyScopeTaken) {
		t.Errorf("second policy for a scope: err=%v", err)
	}
	p1.Strategy = directory.StrategyOrderedFallback
	if err := s.PutPolicy(ctx, p1); err != nil {
		t.Errorf("updating the scope's own policy: %v", err)
	}

	// Seeded defaults are marked; an operator's put clears the mark; a
	// scope the operator already holds is not seeded over.
	if err := s.PutPolicy(ctx, &directory.DispatchPolicy{TenantID: victim, ScopeType: "precedence", ScopeID: "Flash",
		Strategy: directory.StrategyAllBearers}); err != nil {
		t.Fatal(err)
	}
	n, err := s.SeedPrecedenceDefaults(ctx, victim)
	if err != nil || n != len(directory.PrecedenceDefaults)-1 {
		t.Fatalf("seed over an operator's Flash: inserted %d, err %v", n, err)
	}
	list, _ := s.ListPolicies(ctx, victim)
	seeded := 0
	var routine *directory.DispatchPolicy
	for i := range list {
		p := &list[i]
		if p.ScopeType == "precedence" && p.ScopeID == "Flash" {
			if p.Seeded || p.Strategy != directory.StrategyAllBearers {
				t.Errorf("seed overwrote the operator's Flash: %+v", p)
			}
		}
		if p.Seeded {
			seeded++
		}
		if p.ScopeID == "Routine" {
			routine = p
		}
	}
	if seeded != len(directory.PrecedenceDefaults)-1 || routine == nil || !routine.Seeded {
		t.Fatalf("seeded rows: %d, routine %+v", seeded, routine)
	}
	routine.Strategy = directory.StrategyAnyReachable
	if err := s.PutPolicy(ctx, routine); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetPolicy(ctx, victim, routine.ID); got.Seeded || got.Strategy != directory.StrategyAnyReachable {
		t.Errorf("operator's edit of a seeded policy: %+v", got)
	}
	if err := s.DeletePolicy(ctx, victim, p1.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetPolicy(ctx, victim, p1.ID); got != nil {
		t.Errorf("deleted policy still readable: %+v", got)
	}
}

// seedConcurrent: Hub replicas seed the default tenant at the same start;
// each default lands once and the version moves once.
func seedConcurrent(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	const replicas = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.SeedPrecedenceDefaults(ctx, victim)
			if err != nil {
				t.Errorf("seed: %v", err)
				return
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	list, _ := s.ListPolicies(ctx, victim)
	if total != len(directory.PrecedenceDefaults) || len(list) != len(directory.PrecedenceDefaults) {
		t.Errorf("concurrent seeds inserted %d, tenant holds %d, want %d", total, len(list), len(directory.PrecedenceDefaults))
	}
	if v := version(t, s, victim); v != 1 {
		t.Errorf("concurrent seeds: version %d, want 1", v)
	}
}

func guards(t *testing.T, s *directory.SQLStore) {
	ctx := context.Background()
	if _, err := s.GetContact(ctx, "", "x"); !errors.Is(err, directory.ErrEmptyID) {
		t.Errorf("GetContact empty tenant: %v", err)
	}
	if _, err := s.ListContacts(ctx, ""); !errors.Is(err, directory.ErrEmptyID) {
		t.Errorf("ListContacts empty tenant: %v", err)
	}
	if _, err := s.Snapshot(ctx, ""); !errors.Is(err, directory.ErrEmptyID) {
		t.Errorf("Snapshot empty tenant: %v", err)
	}
	if _, err := s.SeedPrecedenceDefaults(ctx, ""); !errors.Is(err, directory.ErrEmptyID) {
		t.Errorf("Seed empty tenant: %v", err)
	}
	if err := s.PutContact(ctx, &directory.Contact{DisplayName: "x"}); !errors.Is(err, directory.ErrEmptyID) {
		t.Errorf("PutContact without tenant: %v", err)
	}
	if _, err := s.FindContactByAddress(ctx, victim, "BOGUS", "x"); !errors.Is(err, directory.ErrInvalidAddressKind) {
		t.Errorf("FindContactByAddress bad kind: %v", err)
	}
	if c, err := s.FindContactByAddress(ctx, victim, directory.KindSMS, "+31699999999"); c != nil || err != nil {
		t.Errorf("unknown address: %+v %v", c, err)
	}
	if c, err := s.GetContact(ctx, victim, "missing"); c != nil || err != nil {
		t.Errorf("missing contact: %+v %v", c, err)
	}
	if g, err := s.GetGroup(ctx, victim, "missing"); g != nil || err != nil {
		t.Errorf("missing group: %+v %v", g, err)
	}
	if p, err := s.GetPolicy(ctx, victim, "missing"); p != nil || err != nil {
		t.Errorf("missing policy: %+v %v", p, err)
	}
}
