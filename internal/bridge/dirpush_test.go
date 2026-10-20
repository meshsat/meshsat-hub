package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"

	_ "modernc.org/sqlite"
)

type pushFakeStore struct {
	mu      sync.Mutex
	bridges map[string][]*store.Bridge // by tenant
}

func (f *pushFakeStore) ListTenants(context.Context) ([]store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Tenant
	for id := range f.bridges {
		out = append(out, store.Tenant{ID: id})
	}
	return out, nil
}

func (f *pushFakeStore) ListBridges(_ context.Context, tenantID string) ([]*store.Bridge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*store.Bridge
	for _, b := range f.bridges[tenantID] {
		cp := *b
		out = append(out, &cp)
	}
	return out, nil
}

// birth sets a bridge's stored birth (a new connect). A v2 bridge reports
// keyID as the key it trusts.
func (f *pushFakeStore) birth(tenant, id string, online bool, version int64, keyID string, caps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(protocol.BridgeBirth{BridgeID: id, TenantID: tenant, Capabilities: caps,
		DirectoryVersion: version, DirectoryKeyID: keyID, Timestamp: time.Now()})
	for _, b := range f.bridges[tenant] {
		if b.BridgeID == id {
			b.Online, b.LastBirth = online, string(raw)
			return
		}
	}
	f.bridges[tenant] = append(f.bridges[tenant], &store.Bridge{BridgeID: id, TenantID: tenant, Online: online, LastBirth: string(raw)})
}

type sent struct {
	bridge string
	cmd    protocol.Command
}

type pushRig struct {
	p      *DirectoryPusher
	st     *pushFakeStore
	dir    *directory.SQLStore
	anchor *directory.TrustAnchor
	kv     kv
	known  map[string][]byte // keys a pushed envelope may be signed by, besides the current one
	clock  time.Time
	mu     sync.Mutex
	sent   []sent
	bad    error // a push that did not open, seen off the test goroutine
	answer func(bridge string, version int64) (*protocol.CommandResponse, error)
}

type kv map[string]string

func (m kv) GetSystemConfig(_ context.Context, k string) (string, error) { return m[k], nil }
func (m kv) SetSystemConfig(_ context.Context, k, v string) error        { m[k] = v; return nil }
func (m kv) CompareAndSetSystemConfig(_ context.Context, k, old, v string) (bool, error) {
	if m[k] != old {
		return false, nil
	}
	m[k] = v
	return true, nil
}

func newPushRig(t *testing.T) *pushRig {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	dir := directory.NewSQLStore(db)
	if err := dir.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := kv{}
	anchor, err := directory.LoadOrCreateTrustAnchor(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	r := &pushRig{st: &pushFakeStore{bridges: map[string][]*store.Bridge{}}, dir: dir, anchor: anchor, kv: keys,
		known: map[string][]byte{}, clock: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	// By default a bridge applies what it is sent and says so.
	r.answer = func(_ string, v int64) (*protocol.CommandResponse, error) {
		res, _ := json.Marshal(DirectoryPushAnswer{Version: v, Applied: true, AppliedVersion: &v})
		return &protocol.CommandResponse{Status: "ok", Result: res}, nil
	}
	r.p = NewDirectoryPusher(r.st, dir, anchor, func(_ context.Context, bridgeID string, cmd protocol.Command) (*protocol.CommandResponse, error) {
		v, _, err := r.open(cmd)
		r.mu.Lock()
		r.sent = append(r.sent, sent{bridgeID, cmd})
		if err != nil && r.bad == nil {
			r.bad = err
		}
		answer := r.answer
		r.mu.Unlock()
		return answer(bridgeID, v)
	})
	r.p.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }
	return r
}

func (r *pushRig) tick() {
	r.p.Tick(context.Background())
	r.p.Wait()
}

// birth is a connect of a bridge that trusts the anchor's current key.
func (r *pushRig) birthTrusting(tenant, id string, version int64, keyID string) {
	r.st.birth(tenant, id, true, version, keyID, protocol.CapDirectoryV2)
}

func (r *pushRig) birth(tenant, id string, online bool, version int64, caps ...string) {
	key := ""
	for _, c := range caps {
		if c == protocol.CapDirectoryV2 {
			key = r.anchor.KeyID()
		}
	}
	r.st.birth(tenant, id, online, version, key, caps...)
}

func (r *pushRig) advance(d time.Duration) { r.mu.Lock(); r.clock = r.clock.Add(d); r.mu.Unlock() }

// take returns and clears what was sent.
func (r *pushRig) take() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bad != nil {
		panic(r.bad) // a push that does not open fails whichever test took it
	}
	out := r.sent
	r.sent = nil
	return out
}

// open verifies a push and returns its version and tenant: a v2 envelope
// under the anchor's key, a v1 snapshot by its v1 signature.
func (r *pushRig) open(cmd protocol.Command) (int64, string, error) {
	if cmd.Cmd != "directory_push" && cmd.Cmd != "directory_rotate" {
		return 0, "", fmt.Errorf("command %q", cmd.Cmd)
	}
	var probe struct {
		V int `json:"v"`
	}
	_ = json.Unmarshal(cmd.Payload, &probe)
	if probe.V == 2 || cmd.Cmd == "directory_rotate" {
		var env directory.Envelope
		if err := json.Unmarshal(cmd.Payload, &env); err != nil {
			return 0, "", err
		}
		der := r.anchor.PublicKey()
		r.mu.Lock()
		known := r.known[env.KeyID]
		r.mu.Unlock()
		payload, err := directory.OpenEnvelope(&env, func(id string) []byte {
			if id == directory.KeyID(der) {
				return der
			}
			return known
		})
		if err != nil {
			return 0, "", err
		}
		var p struct {
			TenantID string `json:"tenant_id"`
			Version  int64  `json:"version"`
		}
		err = json.Unmarshal(payload, &p)
		return p.Version, p.TenantID, err
	}
	snap, err := r.openV1(cmd)
	if err != nil {
		return 0, "", err
	}
	return snap.Version, snap.TenantID, nil
}

func (r *pushRig) openV1(cmd protocol.Command) (directory.Snapshot, error) {
	var snap directory.Snapshot
	if err := json.Unmarshal(cmd.Payload, &snap); err != nil {
		return snap, err
	}
	canon, _ := directory.CanonicalV1(&snap)
	pub, _ := x509.ParsePKIXPublicKey(r.anchor.PublicKey())
	digest := sha256.Sum256(canon)
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), digest[:], snap.Signature) {
		return snap, errors.New("v1 push signature does not verify")
	}
	return snap, nil
}

func (r *pushRig) versionOf(t *testing.T, cmd protocol.Command) int64 {
	t.Helper()
	v, _, err := r.open(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *pushRig) v1(t *testing.T, cmd protocol.Command) directory.Snapshot {
	t.Helper()
	snap, err := r.openV1(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func put(t *testing.T, dir *directory.SQLStore, tenant, name string) {
	t.Helper()
	if err := dir.PutContact(context.Background(), &directory.Contact{TenantID: tenant, DisplayName: name}); err != nil {
		t.Fatal(err)
	}
}

func payloadTenant(t *testing.T, r *pushRig, x sent) string {
	t.Helper()
	_, tenant, err := r.open(x.cmd)
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

func TestDirectoryPushGoesToBridgesBehindTheirTenant(t *testing.T) {
	r := newPushRig(t)
	const victim, other = store.DefaultTenantID, "t-other"
	put(t, r.dir, victim, "A")
	put(t, r.dir, victim, "B") // victim at version 2
	put(t, r.dir, other, "X")  // other at version 1
	r.birth(victim, "kit-1", true, 0, protocol.CapDirectoryV2)
	r.birth(victim, "kit-2", true, 2, protocol.CapDirectoryV2)  // already current
	r.birth(victim, "kit-3", false, 0, protocol.CapDirectoryV2) // offline
	r.birth(other, "kit-9", true, 0, protocol.CapDirectoryV2)

	r.tick()
	got := map[string]string{}
	for _, s := range r.take() {
		got[s.bridge] = payloadTenant(t, r, s)
	}
	if len(got) != 2 || got["kit-1"] != victim || got["kit-9"] != other {
		t.Fatalf("pushed %v, want kit-1 <- %s and kit-9 <- %s only", got, victim, other)
	}
	// Answered applied: nothing more until the directory changes.
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("pushed again with nothing new: %d", len(s))
	}
	put(t, r.dir, victim, "C") // version 3
	r.tick()
	s := r.take()
	if len(s) != 2 || r.versionOf(t, s[0].cmd) != 3 {
		t.Fatalf("after a change: %d pushes", len(s))
	}
}

// TestDirectoryPushDebounces: writes between two ticks go out as one push
// of the last version.
func TestDirectoryPushDebounces(t *testing.T) {
	r := newPushRig(t)
	r.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
	for i := 0; i < 5; i++ {
		put(t, r.dir, "t", fmt.Sprint("c", i))
	}
	r.tick()
	s := r.take()
	if len(s) != 1 || r.versionOf(t, s[0].cmd) != 5 {
		t.Fatalf("%d pushes for five writes", len(s))
	}
}

// TestDirectoryPushCatchesUpAtTheNextBirth: a bridge that missed pushes
// (offline) gets the current snapshot once it is back, and a bridge that
// comes back holding less than it answered before (reset) gets it again.
func TestDirectoryPushCatchesUpAtTheNextBirth(t *testing.T) {
	r := newPushRig(t)
	r.birth("t", "kit", false, 0, protocol.CapDirectoryV2)
	put(t, r.dir, "t", "A")
	put(t, r.dir, "t", "B")
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("pushed to an offline bridge: %d", len(s))
	}
	r.birth("t", "kit", true, 1, protocol.CapDirectoryV2)
	r.tick()
	if s := r.take(); len(s) != 1 || r.versionOf(t, s[0].cmd) != 2 {
		t.Fatalf("catch-up at birth: %d pushes", len(s))
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("pushed after the bridge applied: %d", len(s))
	}
	r.birth("t", "kit", true, 0, protocol.CapDirectoryV2) // reset, came back empty
	r.tick()
	if s := r.take(); len(s) != 1 {
		t.Fatalf("a reset bridge got %d pushes", len(s))
	}
}

// TestDirectoryPushBacksOffAFailingBridge: a bridge that cannot take the
// push is asked again after 30 s, then 60 s, not every tick; one that says
// it already holds the version is not a failure.
func TestDirectoryPushBacksOffAFailingBridge(t *testing.T) {
	r := newPushRig(t)
	r.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
	put(t, r.dir, "t", "A")
	r.answer = func(string, int64) (*protocol.CommandResponse, error) {
		return &protocol.CommandResponse{Status: "error", Error: "directory_push: no pinned trust anchor"}, nil
	}
	steps := []struct {
		after time.Duration
		want  int
	}{{0, 1}, {10 * time.Second, 0}, {21 * time.Second, 1}, {31 * time.Second, 0}, {30 * time.Second, 1}}
	for i, st := range steps {
		r.advance(st.after)
		r.tick()
		if n := len(r.take()); n != st.want {
			t.Fatalf("step %d (+%s): %d pushes, want %d", i, st.after, n, st.want)
		}
	}
	r.answer = func(string, int64) (*protocol.CommandResponse, error) { return nil, errors.New("timeout") }
	r.advance(2 * time.Minute)
	r.tick()
	if n := len(r.take()); n != 1 {
		t.Fatalf("after the backoff: %d", n)
	}
	// A bridge that answers it holds the version already: current, done.
	r.answer = func(_ string, v int64) (*protocol.CommandResponse, error) {
		res, _ := json.Marshal(DirectoryPushAnswer{Version: v, Applied: false, Reason: "not_newer", AppliedVersion: &v})
		return &protocol.CommandResponse{Status: "ok", Result: res}, nil
	}
	r.advance(time.Hour)
	r.tick()
	r.advance(time.Hour)
	r.tick()
	if n := len(r.take()); n != 1 {
		t.Fatalf("a current bridge was asked %d times", n)
	}
}

// TestDirectoryPushV1UntilEveryBridgeVerifiesV2: one bridge of the tenant
// without directory_v2 keeps the tenant on v1, which leaves out the seeded
// defaults; a v1 push with nothing left in it is still sent, since an empty
// full snapshot is how the last row is taken back (r19 C7-R3), and a bridge
// that applied it is not asked again.
func TestDirectoryPushV1UntilEveryBridgeVerifiesV2(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	r.birth("t", "new-kit", true, 0, protocol.CapDirectoryV2)
	r.birth("t", "old-kit", true, 0)
	if _, err := r.dir.SeedPrecedenceDefaults(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	s := r.take()
	if len(s) != 2 {
		t.Fatalf("the v1 push of seeded defaults only went to %d bridges", len(s))
	}
	for _, x := range s {
		if snap := r.v1(t, x.cmd); len(snap.Contacts) != 0 || len(snap.Policies) != 0 || snap.Version == 0 {
			t.Errorf("v1 push to %s: %+v", x.bridge, snap)
		}
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("the applied empty v1 push was sent again: %d", len(s))
	}
	put(t, r.dir, "t", "A")
	if err := r.dir.PutPolicy(ctx, &directory.DispatchPolicy{TenantID: "t", ScopeType: "contact", ScopeID: "x",
		Strategy: directory.StrategyPrimaryOnly}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	s = r.take()
	if len(s) != 2 {
		t.Fatalf("%d pushes", len(s))
	}
	for _, x := range s {
		snap := r.v1(t, x.cmd)
		if len(snap.Contacts) != 1 || len(snap.Policies) != 1 || snap.Policies[0].ScopeID != "x" {
			t.Errorf("v1 push to %s: %d contacts, policies %+v", x.bridge, len(snap.Contacts), snap.Policies)
		}
	}
	// The old kit is upgraded: the tenant goes v2, seeded defaults included.
	r.birth("t", "old-kit", true, 3, protocol.CapDirectoryV2)
	put(t, r.dir, "t", "B")
	r.tick()
	s = r.take()
	if len(s) != 2 {
		t.Fatalf("%d pushes", len(s))
	}
	for _, x := range s {
		var env directory.Envelope
		if json.Unmarshal(x.cmd.Payload, &env) != nil || env.V != 2 {
			t.Errorf("push to %s is not v2", x.bridge)
		}
	}
}

// TestDirectoryPushLeavesPhonesOut: a phone registered as a bridge (mode
// android or ios) gets no push and does not keep its tenant on v1.
func TestDirectoryPushLeavesPhonesOut(t *testing.T) {
	r := newPushRig(t)
	r.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
	r.st.birth("t", "msa-phone", true, 0, "")
	r.st.birth("t", "msi-phone", true, 0, "")
	r.st.mu.Lock()
	for _, b := range r.st.bridges["t"] {
		switch b.BridgeID {
		case "msa-phone":
			b.Mode = "android"
		case "msi-phone":
			b.Mode = "ios"
		default:
			b.Mode = "direct"
		}
	}
	r.st.mu.Unlock()
	put(t, r.dir, "t", "A")
	r.tick()
	s := r.take()
	if len(s) != 1 || s[0].bridge != "kit" {
		t.Fatalf("pushed %v, want the kit only", cmds(s))
	}
	var env directory.Envelope
	if json.Unmarshal(s[0].cmd.Payload, &env) != nil || env.V != 2 {
		t.Errorf("the phones held the tenant on v1")
	}
}

// TestDirectoryPushNeverDoublesAPushInFlight: a tick while a push waits for
// its answer does not send another.
func TestDirectoryPushNeverDoublesAPushInFlight(t *testing.T) {
	r := newPushRig(t)
	r.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
	put(t, r.dir, "t", "A")
	release := make(chan struct{})
	r.answer = func(_ string, v int64) (*protocol.CommandResponse, error) {
		<-release
		res, _ := json.Marshal(DirectoryPushAnswer{Version: v, Applied: true, AppliedVersion: &v})
		return &protocol.CommandResponse{Status: "ok", Result: res}, nil
	}
	r.p.Tick(context.Background())
	put(t, r.dir, "t", "B")
	r.p.Tick(context.Background())
	close(release)
	r.p.Wait()
	if s := r.take(); len(s) != 1 {
		t.Fatalf("%d pushes while one was in flight", len(s))
	}
	r.tick() // version 2 is still owed
	if s := r.take(); len(s) != 1 || r.versionOf(t, s[0].cmd) != 2 {
		t.Fatalf("after the answer: %d pushes", len(s))
	}
}

// rotateKey rotates the Hub's key for tenants; the old key stays known to
// the rig so records signed by it open.
func (r *pushRig) rotateKey(t *testing.T, tenants ...string) *directory.RotationResult {
	t.Helper()
	old := r.anchor.PublicKey()
	r.mu.Lock()
	r.known[directory.KeyID(old)] = old
	r.mu.Unlock()
	res, err := directory.RotateSigningKey(context.Background(), r.kv, r.dir, r.anchor, tenants)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// answerRotations answers a rotate as a bridge that takes it, and a push
// as one that applies it.
func (r *pushRig) answerRotations(rotateOK bool) {
	r.answer = func(_ string, v int64) (*protocol.CommandResponse, error) {
		r.mu.Lock()
		last := r.sent[len(r.sent)-1]
		r.mu.Unlock()
		if last.cmd.Cmd == "directory_rotate" {
			if !rotateOK {
				return &protocol.CommandResponse{Status: "error", Error: "directory_rotate: not signed by the trusted key"}, nil
			}
			var env directory.Envelope
			_ = json.Unmarshal(last.cmd.Payload, &env)
			var p directory.RotationPayload
			_ = json.Unmarshal(env.Payload, &p)
			res, _ := json.Marshal(DirectoryPushAnswer{Version: v, Applied: true, AppliedVersion: &v, KeyID: p.NewKeyID})
			return &protocol.CommandResponse{Status: "ok", Result: res}, nil
		}
		res, _ := json.Marshal(DirectoryPushAnswer{Version: v, Applied: true, AppliedVersion: &v, KeyID: r.anchor.KeyID()})
		return &protocol.CommandResponse{Status: "ok", Result: res}, nil
	}
}

// TestDirectoryPushSendsTheRotationChainFirst: a bridge that trusts a key
// two rotations old gets both records, in order, each signed by the key
// before it, then the snapshot the newest key signed.
func TestDirectoryPushSendsTheRotationChainFirst(t *testing.T) {
	r := newPushRig(t)
	put(t, r.dir, "t", "A") // version 1
	k1 := r.anchor.KeyID()
	r.birthTrusting("t", "kit", 1, k1)
	res1 := r.rotateKey(t, "t") // version 2
	res2 := r.rotateKey(t, "t") // version 3
	put(t, r.dir, "t", "B")     // version 4, signed by the newest key
	r.answerRotations(true)
	r.tick()
	s := r.take()
	if len(s) != 3 || s[0].cmd.Cmd != "directory_rotate" || s[1].cmd.Cmd != "directory_rotate" || s[2].cmd.Cmd != "directory_push" {
		t.Fatalf("sent %d commands: %v", len(s), cmds(s))
	}
	for i, want := range []struct {
		signer, names string
		version       int64
	}{{k1, res1.NewKeyID, 2}, {res1.NewKeyID, res2.NewKeyID, 3}} {
		var env directory.Envelope
		_ = json.Unmarshal(s[i].cmd.Payload, &env)
		var p directory.RotationPayload
		_ = json.Unmarshal(env.Payload, &p)
		if env.KeyID != want.signer || p.NewKeyID != want.names || p.Version != want.version || p.TenantID != "t" {
			t.Errorf("record %d: signed by %s naming %s at %d, want %+v", i, env.KeyID, p.NewKeyID, p.Version, want)
		}
	}
	var env directory.Envelope
	_ = json.Unmarshal(s[2].cmd.Payload, &env)
	if env.KeyID != res2.NewKeyID || r.versionOf(t, s[2].cmd) != 4 {
		t.Errorf("snapshot signed by %s", env.KeyID)
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("a current bridge got %v", cmds(s))
	}
}

// TestDirectoryPushStopsWhereTheBridgeStops: a rotation the bridge does not
// take stops the push (the snapshot would not verify), and a bridge that
// trusts a key the Hub holds no chain from gets nothing.
func TestDirectoryPushStopsWhereTheBridgeStops(t *testing.T) {
	r := newPushRig(t)
	put(t, r.dir, "t", "A")
	r.birthTrusting("t", "kit", 1, r.anchor.KeyID())
	r.birthTrusting("t", "stranger", 0, "0000000000000000000000000000000000000000000000000000000000000000")
	r.rotateKey(t, "t")
	r.answerRotations(false)
	r.tick()
	s := r.take()
	if len(s) != 1 || s[0].bridge != "kit" || s[0].cmd.Cmd != "directory_rotate" {
		t.Fatalf("sent %v", cmds(s))
	}
	r.advance(10 * time.Second)
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("no backoff after a refused rotation: %v", cmds(s))
	}
}

func cmds(s []sent) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.bridge+":"+x.cmd.Cmd)
	}
	return out
}
