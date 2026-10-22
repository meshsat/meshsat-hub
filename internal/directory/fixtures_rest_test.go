package directory

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"slices"
	"testing"
	"time"
)

// fixtures-v1.2 (r40 review, decision 1): directory_v2_rest.json, the C7
// cases with the rows a receiver imports, in the REST shapes (display_name,
// member_ids), made by the Hub's own code: the store (PutContact,
// PutGroup), Snapshot, SnapshotPayload, TrustAnchor.Seal and
// RotateSigningKey, under the fixture's two test keys (the same as v1.1's
// directory_v2.json, which stays as it is: its rows use other names and
// cover the envelope and its signature only).
//
// The store's timestamps are set to one fixed instant, so the payloads are
// the same bytes on every run. The production signer's ECDSA signatures are
// not deterministic: a run keeps the file's signature for a payload that did
// not change, and the test verifies every signature with OpenEnvelope.
// MESHSAT_WRITE_FIXTURES=1 writes the file.

const restFixtureFile = "../../docs/fixtures/v1/directory_v2_rest.json"

var restFixtureTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type restKey struct {
	KeyID   string `json:"key_id"`
	PKIXB64 []byte `json:"pkix_b64"`
}

type restAddress struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type restContact struct {
	ID          string        `json:"id"`
	DisplayName string        `json:"display_name"`
	Addresses   []restAddress `json:"addresses"`
}

type restGroup struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	MemberIDs []string `json:"member_ids"`
}

type restSkip struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// restExpect is the receiver's state after the case, or its refusal.
type restExpect struct {
	Applied    bool          `json:"applied"`
	Reason     string        `json:"reason,omitempty"`
	Pinned     string        `json:"pinned"`
	Version    int64         `json:"version"`
	KeyVersion int64         `json:"key_version"`
	Contacts   []restContact `json:"contacts,omitempty"`
	Groups     []restGroup   `json:"groups,omitempty"`
	Skipped    []restSkip    `json:"skipped,omitempty"`
}

type restCase struct {
	Name       string      `json:"name"`
	Kind       string      `json:"kind"`
	TrustedKey string      `json:"trusted_key"`
	Envelope   *Envelope   `json:"envelope"`
	Verify     bool        `json:"verify"`
	Expect     *restExpect `json:"expect,omitempty"`
	Note       string      `json:"note,omitempty"`
}

type restFixture struct {
	Note      string             `json:"note"`
	Generator string             `json:"generator"`
	Compare   string             `json:"compare"`
	Tenant    string             `json:"tenant"`
	Keys      map[string]restKey `json:"keys"`
	Sequence  []restCase         `json:"sequence"`
	Refused   []restCase         `json:"refused"`
	Rows      []restCase         `json:"rows"`
}

// restTestKey is generate.py's test_key: the scalar is SHA-256("meshsat
// fixtures v1 test key: " + label) mod 2^255, plus one.
func restTestKey(t *testing.T, label string) (*ecdsa.PrivateKey, string) {
	t.Helper()
	seed := sha256.Sum256([]byte("meshsat fixtures v1 test key: " + label))
	d := new(big.Int).SetBytes(seed[:])
	d.Mod(d, new(big.Int).Lsh(big.NewInt(1), 255)).Add(d, big.NewInt(1))
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// The rows as the store must give them back: written here by hand, checked
// against the Hub's own snapshot before anything is written.
var (
	restKitB  = restContact{ID: "c-1", DisplayName: "Kit B", Addresses: []restAddress{{"MQTT", "bridge:kit-b"}, {"TETRA", "tetra:1001"}}}
	restMedic = restContact{ID: "c-2", DisplayName: "Ελένη, field medic", Addresses: []restAddress{{"SMS", "+31600000002"}, {"IRIDIUM_IMT", "300434060000002"}}}
	restRelay = restContact{ID: "c-3", DisplayName: "Relay hill", Addresses: []restAddress{{"MESHTASTIC", "!a1b2c3d4"}}}
	restNine  = restContact{ID: "c-9", DisplayName: "Nine", Addresses: []restAddress{{"SMS", "+31600000009"}}}
)

func restContacts(cs ...restContact) []restContact { return cs }

func TestFixtureDirectoryV2Rest(t *testing.T) {
	ctx := context.Background()
	oldKey, oldPEM := restTestKey(t, "old")
	newKey, newPEM := restTestKey(t, "new")
	pkix := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	keys := map[string]restKey{
		"old": {KeyID: KeyID(pkix(oldKey)), PKIXB64: pkix(oldKey)},
		"new": {KeyID: KeyID(pkix(newKey)), PKIXB64: pkix(newKey)},
	}
	// The v1.1 file's keys, so a receiver can pin either release's.
	if keys["old"].KeyID != "6877951ef0c8eb773f7719262eb91f4d8420ebc418efd726eb1774f42f7b1176" ||
		keys["new"].KeyID != "4a5c6c28c19b0f78db8bb421eb1e35c6f761bf9b4e62eddf42d6e02ce538abdf" {
		t.Fatalf("test keys %s %s are not v1.1's", keys["old"].KeyID, keys["new"].KeyID)
	}

	s := NewSQLStore(openSQLite(t))
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	kv := newFakeStore()
	if err := kv.SetSystemConfig(ctx, systemConfigKey, oldPEM); err != nil {
		t.Fatal(err)
	}
	anchor, err := LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	put := func(c *Contact) {
		t.Helper()
		if err := s.PutContact(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	putGroup := func(g *Group) {
		t.Helper()
		if err := s.PutGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(tenant string) *Snapshot {
		t.Helper()
		snap, err := s.Snapshot(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		snap.SignedAt = restFixtureTime
		for i := range snap.Contacts {
			snap.Contacts[i].CreatedAt, snap.Contacts[i].UpdatedAt = restFixtureTime, restFixtureTime
		}
		for i := range snap.Groups {
			snap.Groups[i].CreatedAt, snap.Groups[i].UpdatedAt = restFixtureTime, restFixtureTime
		}
		return snap
	}
	seal := func(snap *Snapshot) *Envelope {
		t.Helper()
		env, err := anchor.SealSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	check := func(what string, snap *Snapshot, contacts []restContact, groups []restGroup) {
		t.Helper()
		var gotC []restContact
		for _, c := range snap.Contacts {
			rc := restContact{ID: c.ID, DisplayName: c.DisplayName, Addresses: []restAddress{}}
			for _, a := range c.Addresses {
				rc.Addresses = append(rc.Addresses, restAddress{string(a.Kind), a.Value})
			}
			gotC = append(gotC, rc)
		}
		var gotG []restGroup
		for _, g := range snap.Groups {
			gotG = append(gotG, restGroup{ID: g.ID, Name: g.Name, Kind: string(g.Kind), MemberIDs: g.MemberIDs})
		}
		a, _ := json.Marshal([]any{gotC, gotG})
		b, _ := json.Marshal([]any{contacts, groups})
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: the store gives\n%s\nthe fixture says\n%s", what, a, b)
		}
	}

	// Tenant t-1: two contacts, a team and a list.
	put(&Contact{ID: "c-1", TenantID: "t-1", DisplayName: "Kit B", Addresses: []Address{
		{ID: "a-1", Kind: KindMQTT, Value: "bridge:kit-b"}, {ID: "a-2", Kind: KindTETRA, Value: "1001", PrimaryRank: 1}}})
	put(&Contact{ID: "c-2", TenantID: "t-1", DisplayName: "Ελένη, field medic", Addresses: []Address{
		{ID: "a-3", Kind: KindSMS, Value: "+31600000002"}, {ID: "a-4", Kind: KindIridiumIMT, Value: "300434060000002", PrimaryRank: 1}}})
	putGroup(&Group{ID: "g-1", TenantID: "t-1", Name: "North team", Kind: GroupTeam, MemberIDs: []string{"c-1", "c-2"}})
	putGroup(&Group{ID: "g-2", TenantID: "t-1", Name: "All kits", Kind: GroupList, MemberIDs: []string{"c-1"}})
	first := snapshot("t-1")
	g1 := restGroup{ID: "g-1", Name: "North team", Kind: "TEAM", MemberIDs: []string{"c-1", "c-2"}}
	g2 := restGroup{ID: "g-2", Name: "All kits", Kind: "LIST", MemberIDs: []string{"c-1"}}
	check("first", first, restContacts(restKitB, restMedic), []restGroup{g2, g1})
	firstEnv := seal(first)

	// The key rotates (contract C7.6): the record, signed by the old key,
	// takes the next version; then a third contact joins the team under
	// the new key.
	if err := kv.SetSystemConfig(ctx, nextKeyConfig, newPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateSigningKey(ctx, kv, s, anchor, []string{"t-1", "t-2"}); err != nil {
		t.Fatal(err)
	}
	chain, err := s.RotationChain(ctx, "t-1", keys["old"].KeyID)
	if err != nil || len(chain) != 1 {
		t.Fatalf("rotation chain %v %v", chain, err)
	}
	rotEnv := chain[0].Envelope
	put(&Contact{ID: "c-3", TenantID: "t-1", DisplayName: "Relay hill", Addresses: []Address{
		{ID: "a-5", Kind: KindMeshtastic, Value: "!a1b2c3d4"}}})
	g1.MemberIDs = []string{"c-1", "c-2", "c-3"}
	putGroup(&Group{ID: "g-1", TenantID: "t-1", Name: "North team", Kind: GroupTeam, MemberIDs: g1.MemberIDs})
	second := snapshot("t-1")
	check("second", second, restContacts(restKitB, restRelay, restMedic), []restGroup{g2, g1})
	secondEnv := seal(second)

	// Another tenant of the same Hub, under the same key.
	put(&Contact{ID: "c-9", TenantID: "t-2", DisplayName: "Nine", Addresses: []Address{{ID: "a-9", Kind: KindSMS, Value: "+31600000009"}}})
	otherSnap := snapshot("t-2")
	check("other tenant", otherSnap, restContacts(restNine), nil)
	otherEnv := seal(otherSnap)

	// Envelopes the Hub never makes, from its own signer.
	tampered := *firstEnv
	tampered.Payload = bytes.Replace(firstEnv.Payload, []byte(`"Kit B"`), []byte(`"Kit X"`), 1)
	if bytes.Equal(tampered.Payload, firstEnv.Payload) {
		t.Fatal("tamper changed nothing")
	}
	byNewOnly, err := anchor.Seal(rotEnv.Payload)
	if err != nil {
		t.Fatal(err)
	}

	// Rows a receiver skips, each in a snapshot of its own sealed by the
	// new key: the Hub's store refuses to hold them, so they are the
	// second snapshot's payload with one row changed by hand.
	rows := func(name string, change func(p *SnapshotPayloadV2), expect restExpect, note string) restCase {
		var p SnapshotPayloadV2
		if err := json.Unmarshal(secondEnv.Payload, &p); err != nil {
			t.Fatal(err)
		}
		change(&p)
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		env, err := anchor.Seal(raw)
		if err != nil {
			t.Fatal(err)
		}
		expect.Applied, expect.Pinned, expect.Version = true, "new", second.Version
		return restCase{Name: name, Kind: "snapshot", TrustedKey: "new", Envelope: env, Verify: true, Expect: &expect, Note: note}
	}
	origin := HubOrigin("t-1")
	contactsAfter := restContacts(restKitB, restRelay, restMedic) // the store lists by display name
	groupsAfter := []restGroup{g2, g1}

	fx := restFixture{
		Note: "fixtures-v1.2, C7 with the rows a receiver imports. Test keys only (the same as directory_v2.json's). " +
			"Each case says the key the receiver trusts and whether the envelope verifies under it; verify the signature over the decoded payload bytes before parsing them.",
		Generator: "meshsat-hub internal/directory/fixtures_rest_test.go (MESHSAT_WRITE_FIXTURES=1): the Hub's store, Snapshot, SnapshotPayload, " +
			"TrustAnchor.Seal and RotateSigningKey, with the store's timestamps fixed; rows cases change one row of the second snapshot by hand and are sealed by the same signer",
		Compare: "sequence: apply in order to a receiver that pinned keys.old and holds nothing; expect is its state after each step. " +
			"refused: each applied to the state after the whole sequence. rows: each applied to a receiver that pinned keys.new and holds nothing. " +
			"Contacts compare by id with their display_name and the set of their addresses (kind, value); groups by id with name, kind and the set of member_ids; " +
			"skipped compares as a set of (kind, id, reason). A receiver bound to a tenant by its provisioning (a phone) is bound to tenant.",
		Tenant: "t-1",
		Keys:   keys,
		Sequence: []restCase{
			{Name: "the first snapshot, under the old key", Kind: "snapshot", TrustedKey: "old", Envelope: firstEnv, Verify: true,
				Expect: &restExpect{Applied: true, Pinned: "old", Version: first.Version, Contacts: restContacts(restKitB, restMedic), Groups: []restGroup{g2, {ID: "g-1", Name: "North team", Kind: "TEAM", MemberIDs: []string{"c-1", "c-2"}}}}},
			{Name: "the rotation record, signed by the old key", Kind: "rotation", TrustedKey: "old", Envelope: &rotEnv, Verify: true,
				Expect: &restExpect{Applied: true, Pinned: "new", Version: first.Version, KeyVersion: chain[0].Version, Contacts: restContacts(restKitB, restMedic),
					Groups: []restGroup{g2, {ID: "g-1", Name: "North team", Kind: "TEAM", MemberIDs: []string{"c-1", "c-2"}}}}},
			{Name: "the second snapshot, under the new key: a third contact joins the team", Kind: "snapshot", TrustedKey: "new", Envelope: secondEnv, Verify: true,
				Expect: &restExpect{Applied: true, Pinned: "new", Version: second.Version, KeyVersion: chain[0].Version, Contacts: contactsAfter, Groups: groupsAfter}},
		},
		Refused: []restCase{
			{Name: "a payload changed after signing", Kind: "snapshot", TrustedKey: "old", Envelope: &tampered, Verify: false},
			{Name: "the second snapshot under the old key", Kind: "snapshot", TrustedKey: "old", Envelope: secondEnv, Verify: false},
			{Name: "the rotation record signed by the new key only", Kind: "rotation", TrustedKey: "old", Envelope: byNewOnly, Verify: false},
			{Name: "another tenant's snapshot under the same key", Kind: "snapshot", TrustedKey: "new", Envelope: otherEnv, Verify: true,
				Expect: &restExpect{Applied: false, Reason: "other_tenant", Pinned: "new", Version: second.Version, KeyVersion: chain[0].Version, Contacts: contactsAfter, Groups: groupsAfter}},
			{Name: "the second snapshot again", Kind: "snapshot", TrustedKey: "new", Envelope: secondEnv, Verify: true,
				Expect: &restExpect{Applied: false, Reason: "not_newer", Pinned: "new", Version: second.Version, KeyVersion: chain[0].Version, Contacts: contactsAfter, Groups: groupsAfter}},
		},
		Rows: []restCase{
			rows("an address of a kind no receiver knows", func(p *SnapshotPayloadV2) {
				p.Contacts[0].Addresses = append(p.Contacts[0].Addresses, wireAddress{Address: Address{ID: "a-x", Kind: "PIGEON", Value: "loft 7"}, Origin: origin})
			}, restExpect{Contacts: contactsAfter, Groups: groupsAfter, Skipped: []restSkip{{"address", "a-x", "unknown_kind"}}}, ""),
			rows("a contact with no display name", func(p *SnapshotPayloadV2) {
				p.Contacts = append(p.Contacts, wireContact{Contact: Contact{ID: "c-8", TenantID: "t-1", CreatedAt: restFixtureTime, UpdatedAt: restFixtureTime},
					Origin: origin, Addresses: []wireAddress{}})
			}, restExpect{Contacts: contactsAfter, Groups: groupsAfter, Skipped: []restSkip{{"contact", "c-8", "no_display_name"}}}, ""),
			rows("a member that is not a contact of the snapshot", func(p *SnapshotPayloadV2) {
				i := slices.IndexFunc(p.Groups, func(g wireGroup) bool { return g.ID == "g-1" })
				p.Groups[i].MemberIDs = append(p.Groups[i].MemberIDs, "c-404")
			}, restExpect{Contacts: contactsAfter, Groups: groupsAfter, Skipped: []restSkip{{"member", "g-1/c-404", "unknown_member"}}}, ""),
			rows("two contacts with one address", func(p *SnapshotPayloadV2) {
				p.Contacts[1].Addresses = append(p.Contacts[1].Addresses, wireAddress{Address: Address{ID: "a-dup", Kind: KindMQTT, Value: "bridge:kit-b", PrimaryRank: 2}, Origin: origin})
			}, restExpect{Contacts: contactsAfter, Groups: groupsAfter, Skipped: []restSkip{{"address", "a-dup", "address_taken"}}},
				"the first contact in the payload keeps the address"),
			rows("a group of a kind no receiver knows", func(p *SnapshotPayloadV2) {
				p.Groups = append(p.Groups, wireGroup{Group: Group{ID: "g-3", TenantID: "t-1", Name: "Squad", Kind: "SQUAD", MemberIDs: []string{"c-1"},
					CreatedAt: restFixtureTime, UpdatedAt: restFixtureTime}, Origin: origin})
			}, restExpect{Contacts: contactsAfter, Groups: groupsAfter, Skipped: []restSkip{{"group", "g-3", "unknown_kind"}}}, ""),
		},
	}
	if contactsAfter[0].ID != "c-1" || len(fx.Rows) != 5 {
		t.Fatal("the rows cases assume c-1 first")
	}

	// A file already there: keep its signature for every payload that did
	// not change, so the bytes only move when the Hub's output does.
	trusted := func(name string) func(string) []byte {
		return func(id string) []byte {
			if k := keys[name]; k.KeyID == id {
				return k.PKIXB64
			}
			return nil
		}
	}
	all := func(f *restFixture) []*restCase {
		var out []*restCase
		for _, l := range [][]restCase{f.Sequence, f.Refused, f.Rows} {
			for i := range l {
				out = append(out, &l[i])
			}
		}
		return out
	}
	write := os.Getenv("MESHSAT_WRITE_FIXTURES") == "1"
	held, readErr := os.ReadFile(restFixtureFile)
	if readErr == nil {
		var old restFixture
		if err := json.Unmarshal(held, &old); err != nil {
			t.Fatal(err)
		}
		olds := all(&old)
		for i, c := range all(&fx) {
			if i < len(olds) && olds[i].Envelope != nil && olds[i].Envelope.KeyID == c.Envelope.KeyID &&
				bytes.Equal(olds[i].Envelope.Payload, c.Envelope.Payload) {
				env := *c.Envelope
				env.Sig = olds[i].Envelope.Sig
				c.Envelope = &env
			}
		}
	} else if !write || !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("%v (MESHSAT_WRITE_FIXTURES=1 writes it)", readErr)
	}
	for _, c := range all(&fx) {
		_, err := OpenEnvelope(c.Envelope, trusted(c.TrustedKey))
		if (err == nil) != c.Verify {
			t.Errorf("%s: verifies=%v (%v), the case says %v", c.Name, err == nil, err, c.Verify)
		}
	}
	got, err := json.MarshalIndent(fx, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if write {
		if err := os.WriteFile(restFixtureFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		held = got
	}
	if !bytes.Equal(got, held) {
		t.Fatalf("%s differs from what the Hub makes now; MESHSAT_WRITE_FIXTURES=1 writes it, and a change is a new fixture release", restFixtureFile)
	}
}
