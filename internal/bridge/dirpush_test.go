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

// birth sets a bridge's stored birth (a new connect).
func (f *pushFakeStore) birth(tenant, id string, online bool, version int64, caps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(protocol.BridgeBirth{BridgeID: id, TenantID: tenant, Capabilities: caps,
		DirectoryVersion: version, Timestamp: time.Now()})
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
	clock  time.Time
	mu     sync.Mutex
	sent   []sent
	bad    error // a push that did not open, seen off the test goroutine
	answer func(bridge string, version int64) (*protocol.CommandResponse, error)
}

type kv map[string]string

func (m kv) GetSystemConfig(_ context.Context, k string) (string, error) { return m[k], nil }
func (m kv) SetSystemConfig(_ context.Context, k, v string) error        { m[k] = v; return nil }

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
	anchor, err := directory.LoadOrCreateTrustAnchor(context.Background(), kv{})
	if err != nil {
		t.Fatal(err)
	}
	r := &pushRig{st: &pushFakeStore{bridges: map[string][]*store.Bridge{}}, dir: dir, anchor: anchor,
		clock: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
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
	if cmd.Cmd != "directory_push" {
		return 0, "", fmt.Errorf("command %q", cmd.Cmd)
	}
	var probe struct {
		V int `json:"v"`
	}
	_ = json.Unmarshal(cmd.Payload, &probe)
	if probe.V == 2 {
		var env directory.Envelope
		if err := json.Unmarshal(cmd.Payload, &env); err != nil {
			return 0, "", err
		}
		der := r.anchor.PublicKey()
		payload, err := directory.OpenEnvelope(&env, func(id string) []byte {
			if id == directory.KeyID(der) {
				return der
			}
			return nil
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
	r.st.birth(victim, "kit-1", true, 0, protocol.CapDirectoryV2)
	r.st.birth(victim, "kit-2", true, 2, protocol.CapDirectoryV2)  // already current
	r.st.birth(victim, "kit-3", false, 0, protocol.CapDirectoryV2) // offline
	r.st.birth(other, "kit-9", true, 0, protocol.CapDirectoryV2)

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
	r.st.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
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
	r.st.birth("t", "kit", false, 0, protocol.CapDirectoryV2)
	put(t, r.dir, "t", "A")
	put(t, r.dir, "t", "B")
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("pushed to an offline bridge: %d", len(s))
	}
	r.st.birth("t", "kit", true, 1, protocol.CapDirectoryV2)
	r.tick()
	if s := r.take(); len(s) != 1 || r.versionOf(t, s[0].cmd) != 2 {
		t.Fatalf("catch-up at birth: %d pushes", len(s))
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("pushed after the bridge applied: %d", len(s))
	}
	r.st.birth("t", "kit", true, 0, protocol.CapDirectoryV2) // reset, came back empty
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
	r.st.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
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
// defaults; a v1 push with nothing left is not sent.
func TestDirectoryPushV1UntilEveryBridgeVerifiesV2(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	r.st.birth("t", "new-kit", true, 0, protocol.CapDirectoryV2)
	r.st.birth("t", "old-kit", true, 0)
	if _, err := r.dir.SeedPrecedenceDefaults(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("a v1 push of seeded defaults only was sent to %d bridges", len(s))
	}
	r.tick()
	if s := r.take(); len(s) != 0 {
		t.Fatalf("the empty v1 push is not retried: %d", len(s))
	}
	put(t, r.dir, "t", "A")
	if err := r.dir.PutPolicy(ctx, &directory.DispatchPolicy{TenantID: "t", ScopeType: "contact", ScopeID: "x",
		Strategy: directory.StrategyPrimaryOnly}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	s := r.take()
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
	r.st.birth("t", "old-kit", true, 3, protocol.CapDirectoryV2)
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

// TestDirectoryPushNeverDoublesAPushInFlight: a tick while a push waits for
// its answer does not send another.
func TestDirectoryPushNeverDoublesAPushInFlight(t *testing.T) {
	r := newPushRig(t)
	r.st.birth("t", "kit", true, 0, protocol.CapDirectoryV2)
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
