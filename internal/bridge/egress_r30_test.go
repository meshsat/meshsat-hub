package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// gatedStore is a real store whose birth writes can fail once (failNext),
// fail for one bridge until cleared (failBridge), or stop just before the
// store is reached until released (pauseNext): the shape of a retry that
// overlaps a newer birth.
type gatedStore struct {
	store.Store
	mu                  sync.Mutex
	failNext, pauseNext bool
	failBridge          string
	entered, release    chan struct{}
}

func (s *gatedStore) CreateOrUpdateBridge(ctx context.Context, tenant string, b *store.Bridge) error {
	s.mu.Lock()
	fail, pause := s.failNext || s.failBridge == b.BridgeID, s.pauseNext
	s.failNext, s.pauseNext = false, false
	s.mu.Unlock()
	if fail {
		return errors.New("injected birth write failure")
	}
	if pause {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.CreateOrUpdateBridge(ctx, tenant, b)
}

func (s *gatedStore) arm(fail, pause bool) {
	s.mu.Lock()
	s.failNext, s.pauseNext = fail, pause
	if pause {
		s.entered, s.release = make(chan struct{}), make(chan struct{})
	}
	s.mu.Unlock()
}

// replica is one Hub: a subscriber and its wired reader on a shared database.
type replica struct {
	gs   *gatedStore
	sub  *Subscriber
	wire *EgressDeclarations
}

func openSharedDB(t *testing.T, imeis ...string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, imei := range imeis {
		if err := db.CreateDevice(context.Background(), store.DefaultTenantID, &store.Device{IMEI: imei, Label: "modem"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func newReplica(db store.Store) *replica {
	r := &replica{gs: &gatedStore{Store: db}}
	r.sub = NewSubscriber(newMockBus(), r.gs, nil)
	r.wire = NewEgressDeclarations(r.gs)
	r.wire.SetPending(r.sub)
	return r
}

// birthPayload is a birth of bridge naming imei with format ("" for none),
// stamped at.
func birthPayload(t *testing.T, bridge, imei, format string, at time.Time) []byte {
	t.Helper()
	b := protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: bridge, TenantID: "default", Timestamp: at}
	if imei != "" {
		b.Interfaces = []protocol.InterfaceInfo{{Name: "iridium_imt", Type: "iridium_imt", Status: "online", IMEI: imei, EgressFormat: format, EgressSealed: format == "sealed"}}
	}
	p, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *replica) deliver(bridge string, payload []byte) {
	r.sub.handleBridgeBirth("meshsat/bridge/"+bridge+"/birth", payload)
}

// durable is what the database says: the bridge's row (last birth's format
// for the modem), the bridge's stored declaration, the per-modem record and
// the modem's owner.
type durable struct{ birth, decl, record, owner string }

func readDurable(t *testing.T, db *sqlite.DB, bridge, imei string) durable {
	t.Helper()
	ctx := context.Background()
	var d durable
	if row, err := db.GetBridge(ctx, store.DefaultTenantID, bridge); err == nil {
		var b protocol.BridgeBirth
		if err := json.Unmarshal([]byte(row.LastBirth), &b); err != nil {
			t.Fatal(err)
		}
		for _, i := range b.Interfaces {
			if i.IMEI == imei {
				d.birth = i.EgressFormat
			}
		}
	}
	var err error
	if d.decl, err = db.BridgeEgress(ctx, store.DefaultTenantID, bridge, imei); err != nil {
		t.Fatal(err)
	}
	if _, d.record, err = db.DeviceEgress(ctx, store.DefaultTenantID, imei); err != nil {
		t.Fatal(err)
	}
	if d.owner, err = db.DeviceBridgeID(ctx, store.DefaultTenantID, imei); err != nil {
		t.Fatal(err)
	}
	return d
}

func asFormat(d codec.Declaration) string {
	switch d {
	case codec.DeclaredSealed:
		return "sealed"
	case codec.DeclaredPlain:
		return "plain"
	}
	return d.String()
}

// R29-R1 in both directions, in one process: a birth whose write failed is
// written again while a newer birth with the other format lands. Paused just
// before the store, the retry resumes after the newer birth committed, and
// nothing it writes moves the row, the declaration or the record; the hold is
// gone, and the wired and a fresh reader agree on the newer format.
func TestEgressStaleRetryCannotUndoANewerBirth(t *testing.T) {
	for _, tc := range []struct{ old, newer string }{{"plain", "sealed"}, {"sealed", "plain"}} {
		t.Run(tc.old+" retry after "+tc.newer, func(t *testing.T) {
			ctx := context.Background()
			db := openSharedDB(t, egressIMEI)
			r := newReplica(db)
			t0 := time.Now()
			r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, tc.old, t0))
			r.gs.arm(true, false)
			r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, tc.old, t0.Add(time.Second)))
			if _, f, ok := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI); !ok || f != tc.old {
				t.Fatalf("premise: the failed birth is not held: %q %v", f, ok)
			}
			r.gs.arm(false, true)
			done := make(chan struct{})
			go func() { r.sub.ReconcileEgress(ctx); close(done) }()
			<-r.gs.entered
			r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, tc.newer, t0.Add(2*time.Second)))
			if got := readDurable(t, db, "kit-a", egressIMEI); got != (durable{tc.newer, tc.newer, tc.newer, "kit-a"}) {
				t.Fatalf("premise: the newer birth did not land: %+v", got)
			}
			close(r.gs.release)
			<-done
			got := readDurable(t, db, "kit-a", egressIMEI)
			_, _, held := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI)
			wired := asFormat(r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI))
			fresh := asFormat(NewEgressDeclarations(db).Declaration(ctx, store.DefaultTenantID, egressIMEI))
			t.Logf("after the stale retry: birth=%s decl=%s record=%s owner=%s held=%v wired=%s fresh=%s", got.birth, got.decl, got.record, got.owner, held, wired, fresh)
			if got != (durable{tc.newer, tc.newer, tc.newer, "kit-a"}) || held || wired != tc.newer || fresh != tc.newer {
				t.Fatal("an older retry changed what a newer birth stored")
			}
		})
	}
}

// R29-R1 across replicas: two subscribers on one database, as two Hubs. B
// stores every birth; A misses one write and is behind B by one birth. A's
// retry of its held birth, and then A's own late copy of that birth, change
// nothing B stored; while A still holds its older birth, its wired reader
// takes B's newer stored word over its own hold.
func TestEgressTwoSubscribersShareAStore(t *testing.T) {
	for _, tc := range []struct{ old, newer string }{{"plain", "sealed"}, {"sealed", "plain"}} {
		t.Run(tc.old+" then "+tc.newer, func(t *testing.T) {
			ctx := context.Background()
			db := openSharedDB(t, egressIMEI)
			a, b := newReplica(db), newReplica(db)
			t0 := time.Now()
			first := birthPayload(t, "kit-a", egressIMEI, tc.old, t0)
			second := birthPayload(t, "kit-a", egressIMEI, tc.old, t0.Add(time.Second))
			third := birthPayload(t, "kit-a", egressIMEI, tc.newer, t0.Add(2*time.Second))
			a.deliver("kit-a", first)
			b.deliver("kit-a", first)
			a.gs.arm(true, false)
			a.deliver("kit-a", second) // A's write fails: held
			b.deliver("kit-a", second)
			b.deliver("kit-a", third) // A has not received it yet
			if got := asFormat(a.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI)); got != tc.newer {
				t.Fatalf("A's reader took its older hold over B's newer stored word: %s", got)
			}
			a.sub.ReconcileEgress(ctx)
			if got := readDurable(t, db, "kit-a", egressIMEI); got != (durable{tc.newer, tc.newer, tc.newer, "kit-a"}) {
				t.Fatalf("A's retry changed B's newer birth: %+v", got)
			}
			if _, _, held := a.sub.PendingEgress(store.DefaultTenantID, egressIMEI); held {
				t.Fatal("A still holds a birth the store took")
			}
			a.deliver("kit-a", third)
			for name, r := range map[string]*replica{"A": a, "B": b} {
				if got := asFormat(r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI)); got != tc.newer {
					t.Fatalf("%s reads %s", name, got)
				}
			}
			if got := asFormat(NewEgressDeclarations(db).Declaration(ctx, store.DefaultTenantID, egressIMEI)); got != tc.newer {
				t.Fatalf("fresh reader reads %s", got)
			}
		})
	}
}

// R29-R2 across bridges: kit-a's first birth naming the modem fails; the
// modem then turns up in kit-b's birth, received later and stored. While
// kit-a's link is held and kit-b owns the modem, which bridge's word applies
// is unresolved: opaque. kit-a's retry writes its declaration but cannot take
// the modem from the later link, and is let go; the modem reads kit-b's.
func TestEgressRetryDoesNotTakeAModemFromALaterOwner(t *testing.T) {
	ctx := context.Background()
	db := openSharedDB(t, egressIMEI)
	r := newReplica(db)
	t0 := time.Now()
	r.gs.failBridge = "kit-a"
	r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "sealed", t0))
	if got := r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("premise: kit-a's held first birth: %v", got)
	}
	r.deliver("kit-b", birthPayload(t, "kit-b", egressIMEI, "plain", t0.Add(time.Second)))
	if got := r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclarationUnavailable {
		t.Fatalf("kit-a's held link while kit-b owns the modem: %v", got)
	}
	r.gs.mu.Lock()
	r.gs.failBridge = ""
	r.gs.mu.Unlock()
	r.sub.ReconcileEgress(ctx)
	got := readDurable(t, db, "kit-a", egressIMEI)
	if got.owner != "kit-b" || got.decl != "sealed" {
		t.Fatalf("kit-a's retry: %+v", got)
	}
	if _, _, held := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI); held {
		t.Fatal("kit-a's birth is still held after the store took it")
	}
	for name, d := range map[string]*EgressDeclarations{"wired": r.wire, "fresh": NewEgressDeclarations(db)} {
		if got := d.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredPlain {
			t.Fatalf("%s reader after kit-a's retry: %v", name, got)
		}
	}
}

// R29-R2 while the store keeps failing: the first birth naming the modem
// stays held, and the wired reader says sealed, for as long as it takes; the
// first write the store takes links the modem and stores the declaration.
func TestEgressFailedFirstBirthHeldUntilLinked(t *testing.T) {
	ctx := context.Background()
	db := openSharedDB(t, egressIMEI)
	r := newReplica(db)
	t0 := time.Now()
	r.deliver("kit-a", birthPayload(t, "kit-a", "", "", t0)) // before the modem told its IMEI
	r.gs.arm(true, false)
	r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "sealed", t0.Add(time.Second)))
	for i := 0; i < 3; i++ {
		r.gs.arm(true, false)
		r.sub.ReconcileEgress(ctx)
		if _, _, held := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI); !held {
			t.Fatalf("retry %d: a birth the store refused to write was let go", i)
		}
		if got := r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredSealed {
			t.Fatalf("retry %d: wired reader %v", i, got)
		}
	}
	r.sub.ReconcileEgress(ctx)
	if got := readDurable(t, db, "kit-a", egressIMEI); got != (durable{"sealed", "sealed", "sealed", "kit-a"}) {
		t.Fatalf("after the store took it: %+v", got)
	}
	if got := NewEgressDeclarations(db).Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("fresh reader: %v", got)
	}
}

// A held birth written after it has grown older than the stale threshold does
// not mark the bridge online, as a retained replay does not; one written in
// time does.
func TestEgressLateWriteOfAHeldBirthIsNotOnline(t *testing.T) {
	for _, late := range []bool{false, true} {
		db := openSharedDB(t, egressIMEI)
		r := newReplica(db)
		r.sub.SetStaleThreshold(time.Minute)
		at := time.Now()
		if late {
			at = at.Add(-50 * time.Second) // fresh when it came, stale when written
		}
		r.gs.arm(true, false)
		r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "sealed", at))
		if late {
			r.sub.SetStaleThreshold(10 * time.Second)
		}
		r.sub.ReconcileEgress(context.Background())
		row, err := db.GetBridge(context.Background(), store.DefaultTenantID, "kit-a")
		if err != nil {
			t.Fatal(err)
		}
		if row.Online == late {
			t.Fatalf("late=%v: written online=%v", late, row.Online)
		}
	}
}

// One bridge's held birth is written as soon as the store takes another
// bridge's birth, before any tick.
func TestEgressHeldBirthWrittenAfterAnotherBridgesBirth(t *testing.T) {
	const imeiB = "300434060000002"
	db := openSharedDB(t, egressIMEI, imeiB)
	r := newReplica(db)
	t0 := time.Now()
	r.gs.arm(true, false)
	r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "sealed", t0))
	r.deliver("kit-b", birthPayload(t, "kit-b", imeiB, "plain", t0))
	if _, _, held := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI); held {
		t.Fatal("kit-a's birth is still held after the store took kit-b's")
	}
	if got := readDurable(t, db, "kit-a", egressIMEI); got != (durable{"sealed", "sealed", "sealed", "kit-a"}) {
		t.Fatalf("kit-a: %+v", got)
	}
}

// A held birth's write that completes after a newer birth of the same bridge
// was merged in (and failed to write) does not let the newer one go: only the
// write of what is held now releases it.
func TestEgressNewerHeldBirthSurvivesAnOlderWriteCompleting(t *testing.T) {
	ctx := context.Background()
	db := openSharedDB(t, egressIMEI)
	r := newReplica(db)
	t0 := time.Now()
	r.gs.arm(true, false)
	r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "plain", t0))
	r.gs.arm(false, true)
	done := make(chan struct{})
	go func() { r.sub.ReconcileEgress(ctx); close(done) }()
	<-r.gs.entered
	r.gs.arm(true, false)
	r.deliver("kit-a", birthPayload(t, "kit-a", egressIMEI, "sealed", t0.Add(time.Second))) // merged, write fails
	close(r.gs.release)
	<-done // the older snapshot is now written
	if _, f, held := r.sub.PendingEgress(store.DefaultTenantID, egressIMEI); !held || f != "sealed" {
		t.Fatalf("the newer held birth was let go by the older write: %q %v", f, held)
	}
	if got := r.wire.Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("wired reader: %v", got)
	}
	r.sub.ReconcileEgress(ctx)
	if got := readDurable(t, db, "kit-a", egressIMEI); got != (durable{"sealed", "sealed", "sealed", "kit-a"}) {
		t.Fatalf("after the retry: %+v", got)
	}
}

// failingEntries is egressFake whose stored-declaration lookup fails.
type failingEntries struct{ *egressFake }

func (failingEntries) BridgeEgressEntry(context.Context, string, string, string) (store.EgressDecl, error) {
	return store.EgressDecl{}, errors.New("db down")
}

// While lookups fail, a held "sealed" still answers sealed, but a held
// "plain" is not taken on trust: a newer stored word cannot be checked, so
// it is unavailable (opaque) unless this process confirmed one.
func TestEgressHeldPlainIsNotTrustedWhileLookupsFail(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		s    egressStore
	}{
		{"owner lookup fails", &egressFake{ownerErr: errors.New("db down")}},
		{"stored declaration lookup fails", failingEntries{&egressFake{owner: map[string]string{egressIMEI: "kit"}}}},
	} {
		for format, want := range map[string]codec.Declaration{"sealed": codec.DeclaredSealed, "plain": codec.DeclarationUnavailable} {
			d := NewEgressDeclarations(tc.s)
			d.SetPending(heldBy{"kit", format})
			if got := d.Declaration(ctx, "default", egressIMEI); got != want {
				t.Fatalf("%s, held %s: %v, want %v", tc.name, format, got, want)
			}
		}
	}
}

// matrixStore answers every lookup a reader makes for one modem owned by
// "kit": its stored declaration, record and last birth, any of which, or the
// owner lookup, can fail.
type matrixStore struct {
	entry, record string
	fail          string // "owner", "entry", "record", "birth" or ""
}

var errMatrix = errors.New("injected lookup failure")

func (m *matrixStore) DeviceBridgeID(context.Context, string, string) (string, error) {
	if m.fail == "owner" {
		return "", errMatrix
	}
	return "kit", nil
}

func (m *matrixStore) BridgeEgressEntry(context.Context, string, string, string) (store.EgressDecl, error) {
	if m.fail == "entry" {
		return store.EgressDecl{}, errMatrix
	}
	return store.EgressDecl{Format: m.entry}, nil
}

func (m *matrixStore) DeviceEgress(context.Context, string, string) (string, string, error) {
	if m.fail == "record" {
		return "", "", errMatrix
	}
	if m.record == "" {
		return "", "", nil
	}
	return "kit", m.record, nil
}

func (m *matrixStore) GetBridge(context.Context, string, string) (*store.Bridge, error) {
	if m.fail == "birth" {
		return nil, errMatrix
	}
	return &store.Bridge{BridgeID: "kit"}, nil
}

// R30-R1 over every combination: what the reader confirmed before (nothing,
// plain, sealed), what this process holds (nothing, plain, sealed) and which
// lookup fails. Whenever a lookup fails, only a sealed, held or confirmed,
// answers; a plain never does, and the answer is otherwise unavailable. A
// held declaration whose freshness was checked (the lookup that failed came
// after it) answers as held. Read recovery takes the stored word again.
func TestEgressLookupFailureMatrix(t *testing.T) {
	ctx := context.Background()
	for _, cached := range []string{"", "plain", "sealed"} {
		for _, held := range []string{"", "plain", "sealed"} {
			for _, fail := range []string{"owner", "entry", "record", "birth"} {
				m := &matrixStore{entry: cached}
				d := NewEgressDeclarations(m)
				if cached != "" {
					if got := asFormat(d.Declaration(ctx, "default", egressIMEI)); got != cached {
						t.Fatalf("premise: priming %s gave %s", cached, got)
					}
				}
				if held != "" {
					d.SetPending(heldBy{"kit", held})
				}
				// The record and the birth are read only when nothing is
				// stored for the bridge.
				if fail == "record" || fail == "birth" {
					m.entry = ""
				}
				m.fail = fail
				want := codec.DeclarationUnavailable
				switch {
				case held != "" && (fail == "record" || fail == "birth"):
					want = asDeclaration(held)
				case held == "sealed" || cached == "sealed":
					want = codec.DeclaredSealed
				}
				if got := d.Declaration(ctx, "default", egressIMEI); got != want {
					t.Errorf("cached %q, held %q, %s lookup fails: %v, want %v", cached, held, fail, got, want)
				}
				m.fail, m.entry = "", "sealed"
				if got := d.Declaration(ctx, "default", egressIMEI); held == "" && got != codec.DeclaredSealed {
					t.Errorf("cached %q, %s lookup recovered: %v, want the stored sealed", cached, fail, got)
				}
			}
		}
	}
}
