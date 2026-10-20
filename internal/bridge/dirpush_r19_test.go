package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	dto "github.com/prometheus/client_model/go"
)

// pushCount reads the pushes counter for format and result (without
// prometheus/testutil, which is not a dependency of this module).
func pushCount(format, result string) float64 {
	var m dto.Metric
	_ = directoryPushes.WithLabelValues(format, result).Write(&m)
	return m.GetCounter().GetValue()
}

// modelBridge answers directory commands the way the Bridge does since r19
// (meshsat internal/hubreporter + internal/directory): it trusts one key,
// holds a snapshot version and, apart from it, the version of the last
// rotation; an envelope signed by another key is answered unknown_key with
// the key it trusts. The Bridge's own tests replay the Hub's real envelopes
// through its real store (cmd/meshsat directory_rotation_transcript_test.go);
// this model lets the pusher's side of the same cases run here.
type modelBridge struct {
	mu         sync.Mutex
	keyID      string
	keyVersion int64
	version    int64
	rows       []string // display names of the contacts applied
	lose       bool     // the next answer is lost (the command still applies)
	dropPush   bool     // the next snapshot push never arrives (nothing applies)
	last       DirectoryPushAnswer
}

func (m *modelBridge) answer(r *pushRig) func(string, int64) (*protocol.CommandResponse, error) {
	return func(_ string, _ int64) (*protocol.CommandResponse, error) {
		r.mu.Lock()
		last := r.sent[len(r.sent)-1]
		r.mu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.dropPush && last.cmd.Cmd == "directory_push" {
			m.dropPush = false
			return nil, context.DeadlineExceeded
		}
		var env directory.Envelope
		if err := json.Unmarshal(last.cmd.Payload, &env); err != nil || env.V != 2 {
			return &protocol.CommandResponse{Status: "error", Error: "the model takes v2 only"}, nil
		}
		ans := DirectoryPushAnswer{KeyID: m.keyID}
		if env.KeyID != m.keyID {
			ans.Reason = ReasonUnknownKey
		} else if last.cmd.Cmd == "directory_rotate" {
			var p directory.RotationPayload
			_ = json.Unmarshal(env.Payload, &p)
			ans.Version = p.Version
			if p.Version > m.keyVersion {
				m.keyID, m.keyVersion, ans.Applied, ans.KeyID = p.NewKeyID, p.Version, true, p.NewKeyID
			} else {
				ans.Reason = "not_newer"
			}
		} else {
			var snap directory.Snapshot
			_ = json.Unmarshal(env.Payload, &snap)
			ans.Version = snap.Version
			if snap.Version > m.version {
				m.version, ans.Applied = snap.Version, true
				m.rows = nil
				for _, c := range snap.Contacts {
					m.rows = append(m.rows, c.DisplayName)
				}
			} else {
				ans.Reason = "not_newer"
			}
		}
		v, kv := m.version, m.keyVersion
		ans.AppliedVersion, ans.KeyVersion = &v, &kv
		m.last = ans
		if m.lose {
			m.lose = false
			return nil, context.DeadlineExceeded
		}
		res, _ := json.Marshal(ans)
		return &protocol.CommandResponse{Status: "ok", Result: res}, nil
	}
}

func (m *modelBridge) state() (string, int64, int64, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keyID, m.keyVersion, m.version, append([]string(nil), m.rows...)
}

// TestDirectoryPushAfterRotationsAppliesTheSnapshotOfTheSameVersion (r19
// C7-R1): a bridge offline across an edit, a deletion and two rotations gets
// both records and then the snapshot whose version is the last rotation's,
// and applies it; the pusher does not take the rotations' answers for its
// rows.
func TestDirectoryPushAfterRotationsAppliesTheSnapshotOfTheSameVersion(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	put(t, r.dir, "t", "Bob") // version 2
	m := &modelBridge{keyID: r.anchor.KeyID()}
	r.answer = m.answer(r)
	r.birthTrusting("t", "kit", 0, m.keyID)
	r.tick()
	if _, _, v, rows := m.state(); v != 2 || len(rows) != 2 {
		t.Fatalf("initial push: version %d rows %v", v, rows)
	}
	r.take()

	// Offline: an edit, a deletion, two rotations (the last write).
	snap, _ := r.dir.Snapshot(ctx, "t")
	alice, bob := snap.Contacts[0], snap.Contacts[1]
	alice.DisplayName = "Alice, renamed"
	if err := r.dir.PutContact(ctx, &alice); err != nil {
		t.Fatal(err)
	}
	if err := r.dir.DeleteContact(ctx, "t", bob.ID); err != nil {
		t.Fatal(err)
	}
	r.rotateKey(t, "t")
	res2 := r.rotateKey(t, "t")
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	r.st.birth("t", "kit", true, 2, m.keyID, protocol.CapDirectoryV2) // back online
	r.tick()
	key, keyVersion, version, rows := m.state()
	if key != res2.NewKeyID || keyVersion != hubVersion || version != hubVersion {
		t.Fatalf("bridge key %s at %d, rows at %d; Hub at %d with %s", key, keyVersion, version, hubVersion, res2.NewKeyID)
	}
	if len(rows) != 1 || rows[0] != "Alice, renamed" {
		t.Fatalf("rows %v", rows)
	}
	if s := cmds(r.take()); len(s) != 3 {
		t.Fatalf("sent %v; want two rotations and the snapshot", s)
	}
	r.advance(time.Hour)
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("a current bridge got %v", cmds(s))
	}
}

// TestDirectoryPushLostRotationAnswer (r19 C7-R1): the answer to a rotation
// is lost after the bridge took it; the next try sends the same record, the
// bridge answers with the key it already trusts, and the pusher goes on to
// the next record and the snapshot.
func TestDirectoryPushLostRotationAnswer(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice") // version 1
	m := &modelBridge{keyID: r.anchor.KeyID(), version: 1}
	r.st.birth("t", "kit", true, 1, m.keyID, protocol.CapDirectoryV2)
	r.rotateKey(t, "t")
	res2 := r.rotateKey(t, "t")
	put(t, r.dir, "t", "Bob")
	m.lose = true // the first rotation applies, its answer never arrives
	r.answer = m.answer(r)
	r.tick()
	if key, _, _, _ := m.state(); key == res2.NewKeyID {
		t.Fatal("setup: the push went on after a lost answer")
	}
	r.take()
	r.advance(time.Hour) // past the backoff
	r.tick()
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	key, _, version, rows := m.state()
	if key != res2.NewKeyID || version != hubVersion || len(rows) != 2 {
		t.Fatalf("after the retry: key %s rows at %d (%v); want %s at %d", key, version, rows, res2.NewKeyID, hubVersion)
	}
}

// TestDirectoryPushKeyOwedWithCurrentRows (r19 C7-R1): a replica that had
// not reloaded the new key pushed the current snapshot under the old one, so
// the bridge holds the newest rows and the old key; the rotation still
// reaches it, without waiting for another directory edit.
func TestDirectoryPushKeyOwedWithCurrentRows(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	k0 := r.anchor.KeyID()
	res := r.rotateKey(t, "t")
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	m := &modelBridge{keyID: k0, version: hubVersion}
	r.answer = m.answer(r)
	r.st.birth("t", "kit", true, hubVersion, k0, protocol.CapDirectoryV2)
	r.tick()
	if key, keyVersion, version, _ := m.state(); key != res.NewKeyID || keyVersion != hubVersion || version != hubVersion {
		t.Fatalf("bridge key %s at %d, rows at %d", key, keyVersion, version)
	}
	r.take()
	r.advance(time.Hour)
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("a current bridge got %v", cmds(s))
	}
}

// TestDirectoryPushStaleSignerRetries (r19 C7-R1): a pusher whose key is
// older than the one the bridge trusts gets unknown_key, backs off, and
// once it has reloaded the key brings the bridge current.
func TestDirectoryPushStaleSignerRetries(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	stale, err := directory.LoadOrCreateTrustAnchor(ctx, r.kv) // a replica on the old key
	if err != nil {
		t.Fatal(err)
	}
	res := r.rotateKey(t, "t")
	put(t, r.dir, "t", "Bob")
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	m := &modelBridge{keyID: res.NewKeyID, keyVersion: hubVersion - 1, version: 1}
	r.answer = m.answer(r)
	r.p.anchor = stale
	r.st.birth("t", "kit", true, 1, res.NewKeyID, protocol.CapDirectoryV2)
	r.tick()
	if _, _, version, _ := m.state(); version != 1 {
		t.Fatalf("a snapshot signed by the old key applied (%d)", version)
	}
	r.take()
	if err := stale.Refresh(ctx, r.kv); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Hour)
	r.tick()
	if _, _, version, rows := m.state(); version != hubVersion || len(rows) != 2 {
		t.Fatalf("after the reload: rows at %d (%v), Hub at %d", version, rows, hubVersion)
	}
}

// TestDirectoryPushFinalDeletionInAMixedTenant (r19 C7-R3): with an offline
// bridge that keeps the tenant on v1 and seeded defaults present, deleting
// the last contact, group and policy still sends a signed, empty snapshot to
// the online bridge, also after a pusher restart and a reconnect.
func TestDirectoryPushFinalDeletionInAMixedTenant(t *testing.T) {
	ctx := context.Background()
	for _, c := range []string{"same pusher", "pusher restarted", "bridge reconnected"} {
		t.Run(c, func(t *testing.T) {
			r := newPushRig(t)
			if _, err := r.dir.SeedPrecedenceDefaults(ctx, "t"); err != nil {
				t.Fatal(err)
			}
			alice := &directory.Contact{TenantID: "t", DisplayName: "Alice"}
			if err := r.dir.PutContact(ctx, alice); err != nil {
				t.Fatal(err)
			}
			g := &directory.Group{TenantID: "t", Name: "North", Kind: directory.GroupTeam, MemberIDs: []string{alice.ID}}
			if err := r.dir.PutGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
			pol := &directory.DispatchPolicy{TenantID: "t", ScopeType: "contact", ScopeID: alice.ID, Strategy: directory.StrategyPrimaryOnly}
			if err := r.dir.PutPolicy(ctx, pol); err != nil {
				t.Fatal(err)
			}
			r.birth("t", "new-kit", true, 0, protocol.CapDirectoryV2)
			r.birth("t", "old-kit", false, 0)
			r.tick()
			if s := r.take(); len(s) != 1 || len(r.v1(t, s[0].cmd).Contacts) != 1 {
				t.Fatalf("initial push %v", cmds(s))
			}
			for _, err := range []error{r.dir.DeletePolicy(ctx, "t", pol.ID), r.dir.DeleteGroup(ctx, "t", g.ID),
				r.dir.DeleteContact(ctx, "t", alice.ID)} {
				if err != nil {
					t.Fatal(err)
				}
			}
			switch c {
			case "pusher restarted":
				r.p = NewDirectoryPusher(r.st, r.dir, r.anchor, r.p.send)
				r.p.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }
			case "bridge reconnected":
				r.birth("t", "new-kit", true, 1, protocol.CapDirectoryV2)
			}
			hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
			r.tick()
			s := r.take()
			if len(s) != 1 || s[0].bridge != "new-kit" {
				t.Fatalf("after the final deletion: %v", cmds(s))
			}
			snap := r.v1(t, s[0].cmd) // verifies the signature
			if snap.Version != hubVersion || len(snap.Contacts) != 0 || len(snap.Groups) != 0 || len(snap.Policies) != 0 {
				t.Fatalf("revocation push %+v", snap)
			}
		})
	}
}

// TestDirectoryPushRotationReachesAV2BridgeOfAV1Tenant (r19 C7-R1): a tenant
// held on v1 by a bridge that does not verify v2 still has a v2 bridge, which
// takes rotation records; the v1 snapshot is signed by the current key, so
// that bridge must get the records first or it cannot verify the snapshot.
func TestDirectoryPushRotationReachesAV2BridgeOfAV1Tenant(t *testing.T) {
	r := newPushRig(t)
	put(t, r.dir, "t", "Alice")
	k0 := r.anchor.KeyID()
	r.st.birth("t", "new-kit", true, 1, k0, protocol.CapDirectoryV2)
	r.birth("t", "old-kit", false, 0)
	res := r.rotateKey(t, "t")
	put(t, r.dir, "t", "Bob")
	r.answerRotations(true)
	r.tick()
	s := r.take()
	if len(s) != 2 || s[0].cmd.Cmd != "directory_rotate" || s[1].cmd.Cmd != "directory_push" {
		t.Fatalf("sent %v; want the rotation, then the v1 snapshot", cmds(s))
	}
	if snap := r.v1(t, s[1].cmd); len(snap.Contacts) != 2 || r.anchor.KeyID() != res.NewKeyID {
		t.Fatalf("v1 snapshot %+v under %s", snap, r.anchor.KeyID())
	}
}

// TestDirectoryPushSnapshotLostAfterARotation (r19 C7-R1): the bridge takes
// the rotation, the snapshot after it is lost on the way. The rotation was
// the tenant's last write, so its version is the Hub's; the pusher must not
// take the rotation's answer for the rows and must send the snapshot again.
func TestDirectoryPushSnapshotLostAfterARotation(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	put(t, r.dir, "t", "Bob") // version 2
	m := &modelBridge{keyID: r.anchor.KeyID(), version: 1, rows: []string{"Alice"}}
	r.answer = m.answer(r)
	r.st.birth("t", "kit", true, 1, m.keyID, protocol.CapDirectoryV2)
	res := r.rotateKey(t, "t") // version 3, the last write
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	m.dropPush = true
	r.tick()
	if key, _, version, _ := m.state(); key != res.NewKeyID || version != 1 {
		t.Fatalf("setup: key %s, rows at %d", key, version)
	}
	r.take()
	r.advance(time.Hour)
	r.tick()
	if _, _, version, rows := m.state(); version != hubVersion || len(rows) != 2 {
		t.Fatalf("after the retry the bridge holds %d (%v); the Hub is at %d", version, rows, hubVersion)
	}
}

// TestDirectoryPushBridgeAlreadyPastTheChain (r19 C7-R1): a bridge that took
// both rotation records (from an earlier leader whose answers this one never
// saw) is told the first record again; it answers with the newest key and
// the pusher skips to the end of the chain instead of stopping.
func TestDirectoryPushBridgeAlreadyPastTheChain(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	k0 := r.anchor.KeyID()
	r.rotateKey(t, "t")
	res2 := r.rotateKey(t, "t")
	put(t, r.dir, "t", "Bob")
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	m := &modelBridge{keyID: res2.NewKeyID, keyVersion: hubVersion - 1, version: 1}
	r.answer = m.answer(r)
	r.st.birth("t", "kit", true, 1, k0, protocol.CapDirectoryV2) // its birth from before the rotations
	r.tick()
	if s := cmds(r.take()); len(s) != 2 {
		t.Fatalf("sent %v; want the first record, then the snapshot", s)
	}
	if _, _, version, rows := m.state(); version != hubVersion || len(rows) != 2 {
		t.Fatalf("the bridge holds %d (%v); the Hub is at %d", version, rows, hubVersion)
	}
}

// TestDirectoryPushUnknownKeyIsARefusal (r19 C7-R1): a snapshot the bridge
// answers unknown_key (signed by a key it no longer trusts) is counted
// refused, whatever version the bridge holds, never current.
func TestDirectoryPushUnknownKeyIsARefusal(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	put(t, r.dir, "t", "Alice")
	stale, err := directory.LoadOrCreateTrustAnchor(ctx, r.kv)
	if err != nil {
		t.Fatal(err)
	}
	k0 := stale.KeyID()
	res := r.rotateKey(t, "t")
	hubVersion, _ := r.dir.CurrentVersion(ctx, "t")
	m := &modelBridge{keyID: res.NewKeyID, keyVersion: hubVersion, version: hubVersion}
	r.answer = m.answer(r)
	r.p.anchor = stale
	r.st.birth("t", "kit", true, 1, k0, protocol.CapDirectoryV2) // a birth from before the rotation
	refused := pushCount("v2", "refused")
	current := pushCount("v2", "current")
	r.tick()
	if d := pushCount("v2", "refused") - refused; d != 1 {
		t.Errorf("refused +%v; want +1", d)
	}
	if d := pushCount("v2", "current") - current; d != 0 {
		t.Errorf("an unknown_key answer counted current (+%v)", d)
	}
}
