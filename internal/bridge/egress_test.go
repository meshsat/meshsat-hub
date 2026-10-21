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

// egressFake is a declaration store: an owner per modem, a stored birth per
// bridge, the durable record per modem, and errors to inject.
type egressFake struct {
	owner     map[string]string
	birth     map[string]string
	record    map[string][2]string
	ownerErr  error
	birthErr  error
	recordErr error
}

func (f *egressFake) DeviceBridgeID(_ context.Context, _ string, imei string) (string, error) {
	return f.owner[imei], f.ownerErr
}

func (f *egressFake) GetBridge(_ context.Context, _ string, id string) (*store.Bridge, error) {
	if f.birthErr != nil {
		return nil, f.birthErr
	}
	if b, ok := f.birth[id]; ok {
		return &store.Bridge{BridgeID: id, LastBirth: b}, nil
	}
	return nil, nil
}

func (f *egressFake) DeviceEgress(_ context.Context, _ string, imei string) (string, string, error) {
	r := f.record[imei]
	return r[0], r[1], f.recordErr
}

// storedBirth is a stored birth whose one satellite interface carries imei and
// the given declaration fields.
func storedBirth(imei, format string, sealed bool) string {
	b, _ := json.Marshal(protocol.BridgeBirth{BridgeID: "kit", Interfaces: []protocol.InterfaceInfo{
		{Name: "iridium_imt", Type: "iridium_imt", IMEI: imei, EgressFormat: format, EgressSealed: sealed}}})
	return string(b)
}

const egressIMEI = "300434060000001"

func TestEgressDeclarationStates(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		f    egressFake
		want codec.Declaration
	}{
		{"record sealed by the owner", egressFake{owner: map[string]string{egressIMEI: "kit"}, record: map[string][2]string{egressIMEI: {"kit", "sealed"}}}, codec.DeclaredSealed},
		{"record plain by the owner", egressFake{owner: map[string]string{egressIMEI: "kit"}, record: map[string][2]string{egressIMEI: {"kit", "plain"}}}, codec.DeclaredPlain},
		{"record by a bridge that no longer owns the modem", egressFake{owner: map[string]string{egressIMEI: "kit-b"}, record: map[string][2]string{egressIMEI: {"kit", "sealed"}}}, codec.Undeclared},
		{"no record, the owner's birth says plain", egressFake{owner: map[string]string{egressIMEI: "kit"}, birth: map[string]string{"kit": storedBirth(egressIMEI, "plain", false)}}, codec.DeclaredPlain},
		{"no record, the owner's birth has the older flag", egressFake{owner: map[string]string{egressIMEI: "kit"}, birth: map[string]string{"kit": storedBirth(egressIMEI, "", true)}}, codec.DeclaredSealed},
		{"no record, the birth lists the modem without a format", egressFake{owner: map[string]string{egressIMEI: "kit"}, birth: map[string]string{"kit": storedBirth(egressIMEI, "", false)}}, codec.Undeclared},
		{"no owner", egressFake{owner: map[string]string{}}, codec.Undeclared},
		{"owner lookup fails", egressFake{ownerErr: errors.New("db down")}, codec.DeclarationUnavailable},
		{"record lookup fails", egressFake{owner: map[string]string{egressIMEI: "kit"}, recordErr: errors.New("db down")}, codec.DeclarationUnavailable},
		{"birth lookup fails", egressFake{owner: map[string]string{egressIMEI: "kit"}, birthErr: errors.New("db down")}, codec.DeclarationUnavailable},
		{"stored birth unreadable", egressFake{owner: map[string]string{egressIMEI: "kit"}, birth: map[string]string{"kit": "{not json"}}, codec.DeclarationUnavailable},
	} {
		f := c.f
		if got := NewEgressDeclarations(&f).Declaration(ctx, "default", egressIMEI); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	var none *EgressDeclarations
	if got := none.Declaration(ctx, "default", egressIMEI); got != codec.Undeclared {
		t.Errorf("nil reader: %v", got)
	}
}

// What a reader confirmed carries it through a failed lookup, for the same
// owner only; a modem that changed owner is not read by the old declaration.
func TestEgressDeclarationRemembersWhatItConfirmed(t *testing.T) {
	ctx := context.Background()
	f := &egressFake{owner: map[string]string{egressIMEI: "kit"}, record: map[string][2]string{egressIMEI: {"kit", "sealed"}}}
	d := NewEgressDeclarations(f)
	if got := d.Declaration(ctx, "default", egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("confirmed: %v", got)
	}
	f.recordErr = errors.New("db down")
	if got := d.Declaration(ctx, "default", egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("record lookup fails after a confirmed sealed: %v", got)
	}
	f.recordErr, f.ownerErr = nil, errors.New("db down")
	if got := d.Declaration(ctx, "default", egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("owner lookup fails after a confirmed sealed: %v", got)
	}
	f.ownerErr = nil
	f.owner[egressIMEI] = "kit-b"
	f.record = map[string][2]string{}
	if got := d.Declaration(ctx, "default", egressIMEI); got != codec.Undeclared {
		t.Fatalf("a new owner without a declaration is read by the old one's: %v", got)
	}
}

// The birth subscriber's record across a kit's life: incomplete births leave
// it, an explicit plain replaces sealed and back, and a modem that moves to a
// bridge without a declaration is undeclared until that bridge declares.
func TestEgressDeclarationLifecycleThroughBirths(t *testing.T) {
	ms := newMockStore()
	sub := NewSubscriber(newMockBus(), ms, nil)
	d := NewEgressDeclarations(ms)
	ctx := context.Background()
	publish := func(bridgeID string, ifaces ...protocol.InterfaceInfo) {
		t.Helper()
		b, err := json.Marshal(protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: bridgeID, TenantID: "default", Timestamp: time.Now(), Interfaces: ifaces})
		if err != nil {
			t.Fatal(err)
		}
		sub.handleBridgeBirth("meshsat/bridge/"+bridgeID+"/birth", b)
	}
	imt := func(imei, format string) protocol.InterfaceInfo {
		return protocol.InterfaceInfo{Name: "iridium_imt", Type: "iridium_imt", Status: "online", IMEI: imei, EgressFormat: format, EgressSealed: format == "sealed"}
	}
	steps := []struct {
		name string
		do   func()
		want codec.Declaration
	}{
		{"sealed", func() { publish("kit-a", imt(egressIMEI, "sealed")) }, codec.DeclaredSealed},
		{"restart before the IMEI", func() { publish("kit-a", imt("", "sealed")) }, codec.DeclaredSealed},
		{"gateway missing", func() { publish("kit-a") }, codec.DeclaredSealed},
		// R28-R2: a Bridge whose chain read failed sends the IMEI and no format.
		{"IMEI without a format (the chain read failed)", func() { publish("kit-a", imt(egressIMEI, "")) }, codec.DeclaredSealed},
		{"chain changed to plain", func() { publish("kit-a", imt(egressIMEI, "plain")) }, codec.DeclaredPlain},
		{"plain, then the IMEI is late again", func() { publish("kit-a", imt("", "plain")) }, codec.DeclaredPlain},
		{"back to sealed", func() { publish("kit-a", imt(egressIMEI, "sealed")) }, codec.DeclaredSealed},
		{"the modem moves to a bridge that declares nothing", func() {
			publish("phone-b", protocol.InterfaceInfo{Name: "iridium_imt_0", Type: "iridium_imt", IMEI: egressIMEI})
		}, codec.Undeclared},
		{"the new bridge declares sealed", func() { publish("phone-b", imt(egressIMEI, "sealed")) }, codec.DeclaredSealed},
	}
	for _, s := range steps {
		s.do()
		if got := d.Declaration(ctx, "default", egressIMEI); got != s.want {
			t.Fatalf("%s: %v, want %v (owner %q, record %v)", s.name, got, s.want, ms.deviceBridgeMap[egressIMEI], ms.deviceEgress[egressIMEI])
		}
		// A reader with no memory (another replica, a restarted Hub) reads
		// the same from the record alone.
		if got := NewEgressDeclarations(ms).Declaration(ctx, "default", egressIMEI); got != s.want {
			t.Fatalf("%s, fresh reader: %v, want %v (owner %q, record %v)", s.name, got, s.want, ms.deviceBridgeMap[egressIMEI], ms.deviceEgress[egressIMEI])
		}
	}
}

// The record is durable: a Hub that restarts reads the declaration back from
// its database, with no birth in between.
func TestEgressDeclarationSurvivesAReload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	open := func() *sqlite.DB {
		t.Helper()
		db, err := sqlite.New(path, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: egressIMEI, Label: "kit 9704"}); err != nil {
		t.Fatal(err)
	}
	sub := NewSubscriber(newMockBus(), db, nil)
	b, _ := json.Marshal(protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: "kit-a", TenantID: "default", Timestamp: time.Now(),
		Interfaces: []protocol.InterfaceInfo{{Name: "iridium_imt", Type: "iridium_imt", IMEI: egressIMEI, EgressFormat: "sealed", EgressSealed: true}}})
	sub.handleBridgeBirth("meshsat/bridge/kit-a/birth", b)
	// A later birth without the IMEI replaces last_birth; the record stays.
	b, _ = json.Marshal(protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: "kit-a", TenantID: "default", Timestamp: time.Now(),
		Interfaces: []protocol.InterfaceInfo{{Name: "iridium_imt", Type: "iridium_imt", EgressFormat: "sealed", EgressSealed: true}}})
	sub.handleBridgeBirth("meshsat/bridge/kit-a/birth", b)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	t.Cleanup(func() { _ = reopened.Close() })
	by, format, err := reopened.DeviceEgress(ctx, store.DefaultTenantID, egressIMEI)
	if err != nil || by != "kit-a" || format != "sealed" {
		t.Fatalf("record after reload: %q %q %v", by, format, err)
	}
	if got := NewEgressDeclarations(reopened).Declaration(ctx, store.DefaultTenantID, egressIMEI); got != codec.DeclaredSealed {
		t.Fatalf("declaration after reload: %v", got)
	}
}
