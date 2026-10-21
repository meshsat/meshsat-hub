package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// faultyStore is a real store whose declaration writes can be made to fail:
// the per-modem record alone (failRecord), or the birth's write with every
// write after it (failBirth).
type faultyStore struct {
	store.Store
	failRecord, failBirth bool
	ownedElsewhere        bool
}

var errInjected = errors.New("injected write failure")

func (s *faultyStore) SetDeviceEgress(ctx context.Context, tenant, imei, bridge, format string) error {
	if s.failRecord || s.failBirth {
		return errInjected
	}
	return s.Store.SetDeviceEgress(ctx, tenant, imei, bridge, format)
}

func (s *faultyStore) CreateOrUpdateBridge(ctx context.Context, tenant string, b *store.Bridge) error {
	if s.ownedElsewhere {
		return store.ErrOwnedElsewhere
	}
	if s.failBirth {
		return errInjected
	}
	return s.Store.CreateOrUpdateBridge(ctx, tenant, b)
}

type r29Kit struct {
	t    *testing.T
	path string
	db   *sqlite.DB
	fs   *faultyStore
	sub  *Subscriber
	wire *EgressDeclarations // the reader wired to the subscriber, as in main.go
}

func newR29Kit(t *testing.T) *r29Kit {
	t.Helper()
	k := &r29Kit{t: t, path: filepath.Join(t.TempDir(), "hub.db")}
	k.open()
	if err := k.db.CreateDevice(context.Background(), store.DefaultTenantID, &store.Device{IMEI: egressIMEI, Label: "kit 9704"}); err != nil {
		t.Fatal(err)
	}
	return k
}

// open (re)opens the database with a new subscriber and reader: a Hub start.
func (k *r29Kit) open() {
	k.t.Helper()
	db, err := sqlite.New(k.path, 0)
	if err != nil {
		k.t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		k.t.Fatal(err)
	}
	k.db = db
	k.fs = &faultyStore{Store: db}
	k.sub = NewSubscriber(newMockBus(), k.fs, nil)
	k.wire = NewEgressDeclarations(k.fs)
	k.wire.SetPending(k.sub)
	k.t.Cleanup(func() { _ = db.Close() })
}

func (k *r29Kit) restart() {
	k.t.Helper()
	if err := k.db.Close(); err != nil {
		k.t.Fatal(err)
	}
	k.open()
}

// birth publishes a birth from kit-a: format "" with imei "" is a birth sent
// before the modem told its IMEI; gateway false leaves the gateway out.
func (k *r29Kit) birth(imei, format string, gateway bool) {
	k.t.Helper()
	b := protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: "kit-a", TenantID: "default", Timestamp: time.Now()}
	if gateway {
		b.Interfaces = []protocol.InterfaceInfo{{Name: "iridium_imt", Type: "iridium_imt", Status: "online", IMEI: imei, EgressFormat: format, EgressSealed: format == "sealed"}}
	}
	payload, err := json.Marshal(b)
	if err != nil {
		k.t.Fatal(err)
	}
	k.sub.handleBridgeBirth("meshsat/bridge/kit-a/birth", payload)
}

// expect checks the wired reader and a fresh, unwired one on the bare
// database (another replica's view, or this one's after a restart).
func (k *r29Kit) expect(step string, wired, fresh codec.Declaration) {
	k.t.Helper()
	ctx := context.Background()
	if got := k.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != wired {
		k.t.Fatalf("%s: wired reader %v, want %v", step, got, wired)
	}
	if got := NewEgressDeclarations(k.db).Declaration(ctx, store.DefaultTenantID, egressIMEI); got != fresh {
		k.t.Fatalf("%s: fresh reader %v, want %v", step, got, fresh)
	}
}

func (k *r29Kit) record() string {
	k.t.Helper()
	_, format, err := k.db.DeviceEgress(context.Background(), store.DefaultTenantID, egressIMEI)
	if err != nil {
		k.t.Fatal(err)
	}
	return format
}

// R28-R1, the review's case carried on: the per-modem record write fails on
// plain to sealed. The bridge's declarations, saved with the birth, say
// sealed at once to every reader; incomplete births leave it; the record
// converges on the retry; a restart reads sealed from the database alone.
func TestEgressFailedRecordWrite(t *testing.T) {
	k := newR29Kit(t)
	k.birth(egressIMEI, "plain", true)
	k.expect("plain declared", codec.DeclaredPlain, codec.DeclaredPlain)

	k.fs.failRecord = true
	k.birth(egressIMEI, "sealed", true)
	k.fs.failRecord = false
	if k.record() != "plain" {
		t.Fatalf("premise: the record write was meant to fail, record %q", k.record())
	}
	k.expect("record write failed", codec.DeclaredSealed, codec.DeclaredSealed)

	k.birth("", "", true)
	k.expect("then a birth before the IMEI", codec.DeclaredSealed, codec.DeclaredSealed)
	k.birth("", "", false)
	k.expect("then a birth without the gateway", codec.DeclaredSealed, codec.DeclaredSealed)

	k.restart()
	k.expect("after a restart", codec.DeclaredSealed, codec.DeclaredSealed)
	k.birth(egressIMEI, "sealed", true)
	if k.record() != "sealed" {
		t.Fatalf("record after the next full birth: %q", k.record())
	}
}

// The same, with the retry writing the record by itself (no new birth).
func TestEgressFailedRecordWriteIsRetried(t *testing.T) {
	k := newR29Kit(t)
	k.birth(egressIMEI, "plain", true)
	k.fs.failRecord = true
	k.birth(egressIMEI, "sealed", true)
	k.sub.ReconcileEgress(context.Background()) // still failing: stays held
	if _, _, ok := k.sub.PendingEgress(store.DefaultTenantID, egressIMEI); !ok {
		t.Fatal("a declaration whose record write failed was let go")
	}
	k.fs.failRecord = false
	k.sub.ReconcileEgress(context.Background())
	if k.record() != "sealed" {
		t.Fatalf("record after the retry: %q", k.record())
	}
	if _, _, ok := k.sub.PendingEgress(store.DefaultTenantID, egressIMEI); ok {
		t.Fatal("still held after both writes succeeded")
	}
	k.expect("converged", codec.DeclaredSealed, codec.DeclaredSealed)
}

// The whole birth write fails on plain to sealed: the database still says
// plain everywhere. The held declaration makes this process's readers say
// sealed, an incomplete birth meanwhile does not undo it, and the retry
// writes it so that a fresh reader says sealed too.
func TestEgressFailedBirthWrite(t *testing.T) {
	k := newR29Kit(t)
	k.birth(egressIMEI, "plain", true)

	k.fs.failBirth = true
	k.birth(egressIMEI, "sealed", true)
	k.expect("birth write failed", codec.DeclaredSealed, codec.DeclaredPlain)
	k.birth("", "", true)
	k.expect("then a birth before the IMEI, still failing", codec.DeclaredSealed, codec.DeclaredPlain)

	k.fs.failBirth = false
	k.sub.ReconcileEgress(context.Background())
	k.expect("storage back, retried", codec.DeclaredSealed, codec.DeclaredSealed)
	if k.record() != "sealed" {
		t.Fatalf("record after the retry: %q", k.record())
	}
}

// A failed birth write followed by a birth the store takes: the store has
// just taken a write, so the held declaration is written with it, before
// any tick and without a new declaration.
func TestEgressFailedBirthWriteRetriedOnTheNextBirth(t *testing.T) {
	k := newR29Kit(t)
	k.birth(egressIMEI, "plain", true)
	k.fs.failBirth = true
	k.birth(egressIMEI, "sealed", true)
	k.fs.failBirth = false
	k.birth("", "", true)
	k.expect("after the next birth was saved", codec.DeclaredSealed, codec.DeclaredSealed)
	k.restart()
	k.expect("and after a restart", codec.DeclaredSealed, codec.DeclaredSealed)
}

// The loop the subscriber starts writes a held declaration by itself once
// storage takes writes again.
func TestEgressRetryLoopConverges(t *testing.T) {
	old := egressRetryEvery
	egressRetryEvery = 20 * time.Millisecond
	t.Cleanup(func() { egressRetryEvery = old })
	k := newR29Kit(t)
	if err := k.sub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.sub.Stop)
	k.birth(egressIMEI, "plain", true)
	k.fs.failRecord = true
	k.birth(egressIMEI, "sealed", true)
	k.fs.failRecord = false
	deadline := time.Now().Add(5 * time.Second)
	for k.record() != "sealed" {
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not write the record: %q", k.record())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An explicit change to plain still reads plain, also when its record write
// fails; plain is not held back by an older sealed.
func TestEgressExplicitPlainSurvivesAFailedRecordWrite(t *testing.T) {
	k := newR29Kit(t)
	k.birth(egressIMEI, "sealed", true)
	k.fs.failRecord = true
	k.birth(egressIMEI, "plain", true)
	k.fs.failRecord = false
	k.expect("sealed to plain, record write failed", codec.DeclaredPlain, codec.DeclaredPlain)
}

// A birth the store refuses (the bridge belongs to another tenant) declares
// nothing: nothing is held for it.
func TestEgressRefusedBirthHoldsNothing(t *testing.T) {
	k := newR29Kit(t)
	k.fs.ownedElsewhere = true
	k.birth(egressIMEI, "sealed", true)
	if _, _, ok := k.sub.PendingEgress(store.DefaultTenantID, egressIMEI); ok {
		t.Fatal("a refused birth's declaration is held")
	}
}

type heldBy struct{ bridge, format string }

func (h heldBy) PendingEgressEntry(string, string) (string, store.EgressDecl, bool) {
	return h.bridge, store.EgressDecl{Format: h.format}, true
}

// A held birth that links the modem to a bridge other than its owner is not
// written yet: which bridge's word applies is unresolved, so the payload is
// kept opaque (unavailable). The owner's own held declaration answers.
func TestEgressHeldDeclarationOfAnotherOwnerIsUnresolved(t *testing.T) {
	f := &egressFake{owner: map[string]string{egressIMEI: "kit"}, record: map[string][2]string{egressIMEI: {"kit", "plain"}}}
	d := NewEgressDeclarations(f)
	d.SetPending(heldBy{"kit-b", "plain"})
	if got := d.Declaration(context.Background(), "default", egressIMEI); got != codec.DeclarationUnavailable {
		t.Fatalf("another bridge's held link was read as settled: %v", got)
	}
	d.SetPending(heldBy{"kit", "sealed"})
	if got := d.Declaration(context.Background(), "default", egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("the owner's held declaration was not used: %v", got)
	}
}
