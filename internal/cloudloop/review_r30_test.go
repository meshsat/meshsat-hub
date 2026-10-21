package cloudloop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The r30 review's Appendix A, as received.

type reviewR30ReadStore struct {
	store.Store
	failOwner, failEntry bool
}

func (s *reviewR30ReadStore) DeviceBridgeID(ctx context.Context, tenant, imei string) (string, error) {
	if s.failOwner {
		return "", errors.New("injected owner lookup failure")
	}
	return s.Store.DeviceBridgeID(ctx, tenant, imei)
}

func (s *reviewR30ReadStore) BridgeEgressEntry(ctx context.Context, tenant, owner, imei string) (store.EgressDecl, error) {
	if s.failEntry {
		return store.EgressDecl{}, errors.New("injected declaration lookup failure")
	}
	return s.Store.BridgeEgressEntry(ctx, tenant, owner, imei)
}

func TestReviewR30HeldPlainMustNotUseOlderPlainCacheDuringLookupFailure(t *testing.T) {
	for _, failure := range []string{"owner", "entry"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			const imei = "300000000000041"
			db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: "review modem"}); err != nil {
				t.Fatal(err)
			}
			writesA := &reviewR29RetryStore{Store: db}
			readsA := &reviewR30ReadStore{Store: writesA}
			busA, busB := &reviewR28Bus{}, &reviewR28Bus{}
			subA := bridge.NewSubscriber(busA, readsA, nil)
			subB := bridge.NewSubscriber(busB, db, nil)
			for _, sub := range []*bridge.Subscriber{subA, subB} {
				if err := sub.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(sub.Stop)
			}
			publish := func(b *reviewR28Bus, format string, at time.Time) {
				birth := protocol.BridgeBirth{
					Protocol: protocol.ProtocolVersion, BridgeID: "review-kit",
					TenantID: store.DefaultTenantID, Timestamp: at,
					Interfaces: []protocol.InterfaceInfo{{
						Name: "iridium_imt", Type: "iridium_imt", IMEI: imei,
						EgressFormat: format, EgressSealed: format == "sealed",
					}},
				}
				data, err := json.Marshal(birth)
				if err != nil {
					t.Fatal(err)
				}
				b.birth("meshsat/bridge/review-kit/birth", data)
			}
			t0 := time.Now()
			publish(busA, "plain", t0)
			warm := bridge.NewEgressDeclarations(readsA)
			warm.SetPending(subA)
			if got := warm.Declaration(ctx, store.DefaultTenantID, imei); got != codec.DeclaredPlain {
				t.Fatalf("premise: plain was not confirmed: %v", got)
			}
			writesA.mu.Lock()
			writesA.failNextBirth = true
			writesA.mu.Unlock()
			publish(busA, "plain", t0.Add(time.Second))
			publish(busB, "sealed", t0.Add(2*time.Second))
			stored, err := db.BridgeEgressEntry(ctx, store.DefaultTenantID, "review-kit", imei)
			if err != nil || stored.Format != "sealed" {
				t.Fatalf("premise: replica B did not store sealed: %+v %v", stored, err)
			}
			_, held, ok := subA.PendingEgressEntry(store.DefaultTenantID, imei)
			if !ok || held.Format != "plain" || held.Stamp >= stored.Stamp {
				t.Fatalf("premise: replica A did not hold an older plain: %+v %v", held, ok)
			}
			readsA.failOwner = failure == "owner"
			readsA.failEntry = failure == "entry"
			cold := bridge.NewEgressDeclarations(readsA)
			cold.SetPending(subA)
			coldResult := cold.Declaration(ctx, store.DefaultTenantID, imei)
			if coldResult != codec.DeclarationUnavailable {
				t.Fatalf("fresh reader control: %v, want unavailable", coldResult)
			}
			t.Logf("held=%s durable=%s cached_before=plain warm=%s cold=%s", held.Format, stored.Format, warm.Declaration(ctx, store.DefaultTenantID, imei), coldResult)
			h := NewWebhookHandler(busA)
			h.SetEgressDeclarations(warm)
			mo := rnsMO(imei, IMTTopicRaw, reviewR27Ciphertext(t), "00000000-0000-4000-8000-000000000041")
			h.ProcessLingoMO(ctx, mo)
			var decoded WebhookMOMessage
			if err := json.Unmarshal(busA.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
				t.Fatal(err)
			}
			t.Logf("during_lookup_failure opaque=%v compressed=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Compressed, decoded.Text, decoded.Wire == mo.Message)
			if !decoded.Opaque || decoded.Text != "" {
				t.Error("an unverified held plain reused an older plain cache despite a newer sealed declaration")
			}
			readsA.failOwner, readsA.failEntry = false, false
			mo.ID = "00000000-0000-4000-8000-000000000042"
			h.ProcessLingoMO(ctx, mo)
			decoded = WebhookMOMessage{}
			if err := json.Unmarshal(busA.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
				t.Fatal(err)
			}
			t.Logf("after_lookup_recovery opaque=%v text=%q", decoded.Opaque, decoded.Text)
			if !decoded.Opaque || decoded.Text != "" {
				t.Fatal("recovered lookup did not restore the opaque control")
			}
		})
	}
}
