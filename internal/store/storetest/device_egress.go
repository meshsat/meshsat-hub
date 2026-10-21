package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/dbwrap"
)

// The satellite payload format a bridge declared for a modem (HeMB-NG S3-2):
// per registered modem and tenant, replaced only by another declaration, and
// independent of the bridge link and of the bridge's last birth.
func testDeviceEgress(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant, other, imei = "t_egress", "t_egress_other", "300434060000077"
	for _, id := range []string{tenant, other} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	// A modem is one tenant's (the IMEI is unique across tenants).
	if err := s.CreateDevice(ctx, tenant, &store.Device{IMEI: imei, Label: "9704"}); err != nil {
		t.Fatalf("create device: %v", err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "" || format != "" {
		t.Fatalf("before any declaration: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, tenant, imei, "kit-a", "sealed"); err != nil {
		t.Fatal(err)
	}
	// The link moving, or the bridge's birth changing, leaves the record.
	if err := s.AssociateDeviceWithBridge(ctx, tenant, imei, "kit-b"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-a" || format != "sealed" {
		t.Fatalf("after the link moved: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, tenant, imei, "kit-b", "plain"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-b" || format != "plain" {
		t.Fatalf("after a new declaration: %q %q %v", by, format, err)
	}
	// Another tenant can neither read nor write this tenant's modem's record.
	if by, format, err := s.DeviceEgress(ctx, other, imei); err != nil || by != "" || format != "" {
		t.Fatalf("another tenant reads the declaration: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, other, imei, "kit-x", "sealed"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-b" || format != "plain" {
		t.Fatalf("another tenant changed the declaration: %q %q %v", by, format, err)
	}
	// An unregistered modem keeps nothing and answers nothing.
	if err := s.SetDeviceEgress(ctx, tenant, "300434060000099", "kit-a", "sealed"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, "300434060000099"); err != nil || by != "" || format != "" {
		t.Fatalf("unregistered modem: %q %q %v", by, format, err)
	}
}

// What a birth writes besides its row, and the order every part keeps
// (HeMB-NG S3-2, r30): the row and each declaration by the birth's stamp, so
// a birth written later than a newer one (a retry, a replica behind) changes
// neither, in either direction, while a modem it alone declares is taken; a
// birth without a modem keeps that modem's declaration; the links by when
// they were made, so a retry cannot take a modem a later birth or a delete
// moved; the per-modem record follows the bridge's stored declaration; and
// another tenant can neither read nor write any of it.
func testBridgeEgress(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant, other = "t_begress", "t_begress_other"
	const a, b, c = "300434060000081", "300434060000082", "300434060000083"
	const kit, kitY = "kit-begress", "kit-begress-y"
	for _, id := range []string{tenant, other} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	for _, imei := range []string{a, b} {
		if err := s.CreateDevice(ctx, tenant, &store.Device{IMEI: imei, Label: "modem " + imei}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(tn, bridge, imei string) string {
		t.Helper()
		f, err := s.BridgeEgress(ctx, tn, bridge, imei)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	owner := func(imei string) string {
		t.Helper()
		id, err := s.DeviceBridgeID(ctx, tenant, imei)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// lastStamp is the stamp written into the row's last_birth by save.
	lastStamp := func(bridge string) int64 {
		t.Helper()
		row, err := s.GetBridge(ctx, tenant, bridge)
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Stamp int64 }
		if err := json.Unmarshal([]byte(row.LastBirth), &v); err != nil {
			t.Fatal(err)
		}
		return v.Stamp
	}
	type decl = map[string]store.EgressDecl
	save := func(tn, bridge string, stamp int64, egress decl, links map[string]int64) error {
		t.Helper()
		for imei, e := range egress {
			e.Stamp = stamp
			egress[imei] = e
		}
		return s.CreateOrUpdateBridge(ctx, tn, &store.Bridge{
			BridgeID: bridge, Label: bridge, LastBirth: fmt.Sprintf(`{"stamp":%d}`, stamp),
			BirthStamp: stamp, Egress: egress, Links: links,
		})
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	sealed, plain := store.EgressDecl{Format: "sealed"}, store.EgressDecl{Format: "plain"}

	if got := get(tenant, kit, a); got != "" {
		t.Fatalf("unknown bridge: %q", got)
	}
	must(save(tenant, kit, 10, decl{a: sealed}, nil))
	if got := get(tenant, kit, a); got != "sealed" {
		t.Fatalf("first birth: %q", got)
	}
	must(save(tenant, kit, 20, nil, nil)) // a birth that declares nothing
	if got := get(tenant, kit, a); got != "sealed" || lastStamp(kit) != 20 {
		t.Fatalf("a birth without the modem: %q, row at %d", got, lastStamp(kit))
	}
	must(save(tenant, kit, 30, decl{b: plain}, nil))
	if got, gotB := get(tenant, kit, a), get(tenant, kit, b); got != "sealed" || gotB != "plain" {
		t.Fatalf("a birth naming another modem: %q %q", got, gotB)
	}
	must(save(tenant, kit, 40, decl{a: plain}, nil))
	if got := get(tenant, kit, a); got != "plain" {
		t.Fatalf("explicit plain: %q", got)
	}

	// An older birth written after a newer one: neither the row nor a
	// declaration the newer one made moves, in either direction.
	must(save(tenant, kit, 35, decl{a: sealed}, nil))
	if got := get(tenant, kit, a); got != "plain" || lastStamp(kit) != 40 {
		t.Fatalf("an older sealed birth undid a newer plain: %q, row at %d", got, lastStamp(kit))
	}
	must(save(tenant, kit, 50, decl{a: sealed}, nil))
	must(save(tenant, kit, 45, decl{a: plain}, nil))
	if got := get(tenant, kit, a); got != "sealed" || lastStamp(kit) != 50 {
		t.Fatalf("an older plain birth undid a newer sealed: %q, row at %d", got, lastStamp(kit))
	}
	if e, err := s.BridgeEgressEntry(ctx, tenant, kit, a); err != nil || e != (store.EgressDecl{Format: "sealed", Stamp: 50}) {
		t.Fatalf("entry: %+v %v", e, err)
	}
	// ... but a modem only the older birth declares is taken: the newer
	// births left it out, they did not declare otherwise.
	must(save(tenant, kit, 42, decl{c: plain}, nil))
	if got := get(tenant, kit, c); got != "plain" || lastStamp(kit) != 50 {
		t.Fatalf("a declaration only an older birth made: %q, row at %d", got, lastStamp(kit))
	}

	// Tenants: another tenant neither reads nor writes the bridge.
	if got := get(other, kit, a); got != "" {
		t.Fatalf("another tenant reads the bridge's declarations: %q", got)
	}
	if err := save(other, kit, 60, decl{a: plain}, map[string]int64{a: 60}); !errors.Is(err, store.ErrOwnedElsewhere) {
		t.Fatalf("another tenant's birth for the bridge: %v", err)
	}
	if got := get(tenant, kit, a); got != "sealed" || lastStamp(kit) != 50 {
		t.Fatalf("a refused birth wrote: %q, row at %d", got, lastStamp(kit))
	}

	// Links, ordered by when they were made.
	must(save(tenant, kit, 70, nil, map[string]int64{a: 100}))
	if got := owner(a); got != kit {
		t.Fatalf("an unlinked modem: owner %q", got)
	}
	must(save(tenant, kitY, 1, nil, map[string]int64{a: 200}))
	if got := owner(a); got != kitY {
		t.Fatalf("a later link: owner %q", got)
	}
	must(save(tenant, kit, 80, nil, map[string]int64{a: 150}))
	if got := owner(a); got != kitY {
		t.Fatalf("an earlier link took the modem from a later one: owner %q", got)
	}
	must(s.AssociateDeviceWithBridge(ctx, tenant, a, kit)) // a link made now
	must(save(tenant, kitY, 2, nil, map[string]int64{a: 300}))
	if got := owner(a); got != kit {
		t.Fatalf("a link received before the one made now took the modem: owner %q", got)
	}
	before := time.Now().UnixNano()
	must(s.DeleteBridge(ctx, tenant, kit))
	if got, gotOwner := get(tenant, kit, a), owner(a); got != "" || gotOwner != "" {
		t.Fatalf("after the bridge was deleted: declaration %q, owner %q", got, gotOwner)
	}
	must(save(tenant, kit, 90, decl{a: sealed}, map[string]int64{a: before}))
	if got := owner(a); got != "" {
		t.Fatalf("a birth received before the delete linked the modem again: owner %q", got)
	}
	must(save(tenant, kit, 91, nil, map[string]int64{a: time.Now().UnixNano()}))
	if got := owner(a); got != kit {
		t.Fatalf("a birth after the delete: owner %q", got)
	}
	// A modem with no device row takes no link and is no error.
	must(save(tenant, kit, 92, nil, map[string]int64{"300434060000099": time.Now().UnixNano()}))
	if id, err := s.DeviceBridgeID(ctx, tenant, "300434060000099"); err != nil || id != "" {
		t.Fatalf("unregistered modem: %q %v", id, err)
	}

	// The record follows the bridge's stored declaration, whatever format a
	// late call passes; it takes the one passed only when the bridge has none.
	must(s.SetDeviceEgress(ctx, tenant, a, kit, "plain"))
	if by, f, err := s.DeviceEgress(ctx, tenant, a); err != nil || by != kit || f != "sealed" {
		t.Fatalf("a late plain record over a stored sealed: %q %q %v", by, f, err)
	}
	must(s.SetDeviceEgress(ctx, tenant, b, kitY, "plain"))
	if by, f, err := s.DeviceEgress(ctx, tenant, b); err != nil || by != kitY || f != "plain" {
		t.Fatalf("a record where the bridge has no declaration: %q %q %v", by, f, err)
	}
}

// Births of one bridge written at the same time, in any order, as replicas
// and retries do: what stays is the newest birth's row, declaration and link,
// never whichever transaction committed last. [MESHSAT-1584]
func testBirthOrderConcurrent(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant, imei, kit = "t_border", "300434060000085", "kit-border"
	if err := s.CreateTenant(ctx, &store.Tenant{ID: tenant, Slug: tenant, Name: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDevice(ctx, tenant, &store.Device{IMEI: imei, Label: "modem"}); err != nil {
		t.Fatal(err)
	}
	const n = 16
	formats := [2]string{"sealed", "plain"}
	order := []int{9, 3, 16, 1, 12, 7, 5, 14, 2, 11, 8, 15, 4, 10, 6, 13}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, i := range order {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stamp := int64(i)
			errs <- dbwrap.RetryShort(ctx, func() error {
				return s.CreateOrUpdateBridge(ctx, tenant, &store.Bridge{
					BridgeID: kit, Label: kit, LastBirth: fmt.Sprintf(`{"stamp":%d}`, stamp), BirthStamp: stamp,
					Egress: map[string]store.EgressDecl{imei: {Format: formats[i%2], Stamp: stamp}},
					Links:  map[string]int64{imei: stamp},
				})
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	e, err := s.BridgeEgressEntry(ctx, tenant, kit, imei)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.GetBridge(ctx, tenant, kit)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Stamp int64 }
	if err := json.Unmarshal([]byte(row.LastBirth), &v); err != nil {
		t.Fatal(err)
	}
	if e != (store.EgressDecl{Format: formats[n%2], Stamp: n}) || v.Stamp != n {
		t.Fatalf("after %d concurrent births: declaration %+v, row at %d", n, e, v.Stamp)
	}
	if id, err := s.DeviceBridgeID(ctx, tenant, imei); err != nil || id != kit {
		t.Fatalf("owner %q %v", id, err)
	}
}
