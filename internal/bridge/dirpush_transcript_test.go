package bridge

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// The directory transcript for the Bridge (r19 C7-R1, C7-R3): the commands
// the real pusher sends one bridge, in order, over the cases the review
// named, signed by the Hub's real keys and rotations, with the answer the
// model bridge gave each. The Bridge replays them through its real handler
// and store (meshsat cmd/meshsat/directory_rotation_transcript_test.go) and
// checks its own answers equal these and its rows equal the Hub's. Write it
// with MESHSAT_DIRECTORY_TRANSCRIPT=<path> go test -run
// TestDirectoryTranscriptForTheBridge ./internal/bridge/. [MESHSAT-1584]

type transcriptStep struct {
	Cmd     string               `json:"cmd"`
	Payload json.RawMessage      `json:"payload"`
	Lost    bool                 `json:"lost,omitempty"` // the Hub never got the answer
	Expect  *DirectoryPushAnswer `json:"expect"`
}

type transcriptAddress struct {
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Contact string `json:"contact"`
}

type transcriptScenario struct {
	Name       string              `json:"name"`
	Tenant     string              `json:"tenant"`
	InitialKey []byte              `json:"initial_key_pkix"`
	Steps      []transcriptStep    `json:"steps"`
	KeyID      string              `json:"key_id"`      // the key the bridge trusts at the end
	Version    int64               `json:"version"`     // the snapshot it holds
	KeyVersion int64               `json:"key_version"` // the rotation its key is from
	Contacts   map[string]string   `json:"contacts"`    // id -> display name, every Hub contact
	Addresses  []transcriptAddress `json:"addresses"`   // every Hub address and its contact
	Gone       []transcriptAddress `json:"gone"`        // addresses deleted on the Hub
	Groups     map[string][]string `json:"groups"`      // id -> member ids
}

// record wraps the rig's answer so every command sent is a step, with the
// answer the bridge gave (lost or not).
func record(r *pushRig, answer func(string, int64) (*protocol.CommandResponse, error), last func() *DirectoryPushAnswer, steps *[]transcriptStep) {
	r.answer = func(b string, v int64) (*protocol.CommandResponse, error) {
		resp, err := answer(b, v)
		r.mu.Lock()
		cmd := r.sent[len(r.sent)-1].cmd
		r.mu.Unlock()
		*steps = append(*steps, transcriptStep{Cmd: cmd.Cmd, Payload: cmd.Payload, Lost: err != nil, Expect: last()})
		return resp, err
	}
}

func (sc *transcriptScenario) finish(t *testing.T, r *pushRig, m *modelBridge, gone []transcriptAddress) {
	t.Helper()
	ctx := context.Background()
	snap, err := r.dir.Snapshot(ctx, sc.Tenant)
	if err != nil {
		t.Fatal(err)
	}
	sc.Contacts, sc.Groups, sc.Gone = map[string]string{}, map[string][]string{}, gone
	for _, c := range snap.Contacts {
		sc.Contacts[c.ID] = c.DisplayName
		for _, a := range c.Addresses {
			sc.Addresses = append(sc.Addresses, transcriptAddress{Kind: string(a.Kind), Value: a.Value, Contact: c.ID})
		}
	}
	for _, g := range snap.Groups {
		members := append([]string{}, g.MemberIDs...)
		sort.Strings(members)
		sc.Groups[g.ID] = members
	}
	if m != nil {
		sc.KeyID, sc.KeyVersion, sc.Version, _ = m.state()
		if sc.Version != snap.Version {
			t.Fatalf("%s: the model holds %d, the Hub is at %d", sc.Name, sc.Version, snap.Version)
		}
	}
}

func contactWith(t *testing.T, r *pushRig, tenant, name, kind, value string) *directory.Contact {
	t.Helper()
	c := &directory.Contact{TenantID: tenant, DisplayName: name, Org: "Hub org", Role: "Hub role",
		Addresses: []directory.Address{{Kind: directory.AddressKind(kind), Value: value}}}
	if err := r.dir.PutContact(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDirectoryTranscriptForTheBridge(t *testing.T) {
	ctx := context.Background()
	var out []transcriptScenario

	// 1. Offline across an edit, a deletion, an address that moved and two
	// rotations; the snapshot that follows has the last rotation's version.
	{
		r := newPushRig(t)
		sc := transcriptScenario{Name: "offline across edits, a deletion and two rotations", Tenant: "t", InitialKey: r.anchor.PublicKey()}
		alice := contactWith(t, r, "t", "Alice", "SMS", "+31600000001")
		bob := contactWith(t, r, "t", "Bob", "SMS", "+31600000002")
		team := &directory.Group{TenantID: "t", Name: "North", Kind: directory.GroupTeam, MemberIDs: []string{alice.ID, bob.ID}}
		if err := r.dir.PutGroup(ctx, team); err != nil {
			t.Fatal(err)
		}
		m := &modelBridge{keyID: r.anchor.KeyID()}
		record(r, m.answer(r), func() *DirectoryPushAnswer { a := m.last; return &a }, &sc.Steps)
		r.birthTrusting("t", "kit", 0, m.keyID)
		r.tick()
		r.st.birth("t", "kit", false, 0, m.keyID, protocol.CapDirectoryV2) // offline
		alice.DisplayName = "Alice, renamed offline"
		alice.Addresses = append(alice.Addresses, directory.Address{Kind: "TETRA", Value: "1001"})
		if err := r.dir.PutContact(ctx, alice); err != nil {
			t.Fatal(err)
		}
		if err := r.dir.DeleteContact(ctx, "t", bob.ID); err != nil {
			t.Fatal(err)
		}
		carol := contactWith(t, r, "t", "Carol", "SMS", "+31600000002") // Bob's number, now Carol's
		team.MemberIDs = []string{alice.ID, carol.ID}
		if err := r.dir.PutGroup(ctx, team); err != nil {
			t.Fatal(err)
		}
		r.rotateKey(t, "t")
		r.rotateKey(t, "t")
		r.st.birth("t", "kit", true, 1, m.keyID, protocol.CapDirectoryV2) // back, holding the first push
		r.tick()
		sc.finish(t, r, m, nil)
		if len(sc.Steps) != 4 {
			t.Fatalf("%s: %d steps", sc.Name, len(sc.Steps))
		}
		out = append(out, sc)
	}

	// 2. The answer to the first of two rotations is lost.
	{
		r := newPushRig(t)
		sc := transcriptScenario{Name: "the answer to a rotation is lost", Tenant: "t", InitialKey: r.anchor.PublicKey()}
		contactWith(t, r, "t", "Alice", "SMS", "+31600000001")
		m := &modelBridge{keyID: r.anchor.KeyID()}
		record(r, m.answer(r), func() *DirectoryPushAnswer { a := m.last; return &a }, &sc.Steps)
		r.birthTrusting("t", "kit", 0, m.keyID)
		r.tick()
		r.rotateKey(t, "t")
		r.rotateKey(t, "t")
		contactWith(t, r, "t", "Bob", "SMS", "+31600000002")
		m.lose = true
		r.tick()
		r.advance(time.Hour)
		r.tick()
		sc.finish(t, r, m, nil)
		if !sc.Steps[1].Lost || sc.Steps[2].Expect.Reason != ReasonUnknownKey {
			t.Fatalf("%s: steps %+v", sc.Name, sc.Steps)
		}
		out = append(out, sc)
	}

	// 3. A replica that had not reloaded the new key pushed the newest
	// snapshot under the old one; the rotation, older than that snapshot,
	// still reaches the bridge.
	{
		r := newPushRig(t)
		sc := transcriptScenario{Name: "a replica signs the newest snapshot with the old key", Tenant: "t", InitialKey: r.anchor.PublicKey()}
		contactWith(t, r, "t", "Alice", "SMS", "+31600000001")
		m := &modelBridge{keyID: r.anchor.KeyID()}
		record(r, m.answer(r), func() *DirectoryPushAnswer { a := m.last; return &a }, &sc.Steps)
		r.birthTrusting("t", "kit", 0, m.keyID)
		r.tick()
		stale, err := directory.LoadOrCreateTrustAnchor(ctx, r.kv)
		if err != nil {
			t.Fatal(err)
		}
		fresh := r.anchor
		r.rotateKey(t, "t")
		contactWith(t, r, "t", "Bob", "SMS", "+31600000002")
		r.p.anchor = stale
		r.advance(time.Hour)
		r.tick()
		r.p.anchor = fresh
		r.advance(time.Hour)
		r.tick()
		sc.finish(t, r, m, nil)
		if len(sc.Steps) != 4 || sc.Steps[2].Cmd != "directory_rotate" || !sc.Steps[2].Expect.Applied {
			t.Fatalf("%s: steps %+v", sc.Name, sc.Steps)
		}
		out = append(out, sc)
	}

	// 4. A tenant held on v1 by an offline bridge: the last contact, group
	// and policy are deleted and the empty snapshot takes them back.
	{
		r := newPushRig(t)
		sc := transcriptScenario{Name: "the final deletion in a tenant on v1", Tenant: "t", InitialKey: r.anchor.PublicKey()}
		if _, err := r.dir.SeedPrecedenceDefaults(ctx, "t"); err != nil {
			t.Fatal(err)
		}
		alice := contactWith(t, r, "t", "Alice", "SMS", "+31600000001")
		g := &directory.Group{TenantID: "t", Name: "North", Kind: directory.GroupTeam, MemberIDs: []string{alice.ID}}
		if err := r.dir.PutGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
		pol := &directory.DispatchPolicy{TenantID: "t", ScopeType: "contact", ScopeID: alice.ID, Strategy: directory.StrategyPrimaryOnly}
		if err := r.dir.PutPolicy(ctx, pol); err != nil {
			t.Fatal(err)
		}
		var last DirectoryPushAnswer
		record(r, func(_ string, v int64) (*protocol.CommandResponse, error) {
			last = DirectoryPushAnswer{TenantID: "t", Version: v, Applied: true, AppliedVersion: &v, KeyID: r.anchor.KeyID()}
			res, _ := json.Marshal(last)
			return &protocol.CommandResponse{Status: "ok", Result: res}, nil
		}, func() *DirectoryPushAnswer { a := last; return &a }, &sc.Steps)
		r.birth("t", "new-kit", true, 0, protocol.CapDirectoryV2)
		r.birth("t", "old-kit", false, 0)
		r.tick()
		for _, err := range []error{r.dir.DeletePolicy(ctx, "t", pol.ID), r.dir.DeleteGroup(ctx, "t", g.ID),
			r.dir.DeleteContact(ctx, "t", alice.ID)} {
			if err != nil {
				t.Fatal(err)
			}
		}
		r.tick()
		sc.finish(t, r, nil, []transcriptAddress{{Kind: "SMS", Value: "+31600000001", Contact: alice.ID}})
		v, _ := r.dir.CurrentVersion(ctx, "t")
		sc.KeyID, sc.Version = r.anchor.KeyID(), v
		if len(sc.Steps) != 2 || len(sc.Contacts) != 0 {
			t.Fatalf("%s: %d steps, contacts %v", sc.Name, len(sc.Steps), sc.Contacts)
		}
		out = append(out, sc)
	}

	if path := os.Getenv("MESHSAT_DIRECTORY_TRANSCRIPT"); path != "" {
		raw, err := json.MarshalIndent(out, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d scenarios to %s", len(out), path)
	}
}
