package cloudloop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The r29 review's Appendix A. One change: the retry now writes a held birth
// through CreateOrUpdateBridge (the row, declarations and links in one
// transaction) and no longer calls MergeBridgeEgress, so the barrier that
// paused the retry immediately before its durable write moved from the
// wrapper's MergeBridgeEgress into its CreateOrUpdateBridge, armed the same
// way (pausePlain) and taken by the first write after arming. The scenario,
// the assertions and the log lines are the review's.
type reviewR29RetryStore struct {
	store.Store
	mu            sync.Mutex
	failNextBirth bool
	pausePlain    bool
	entered       chan struct{}
	release       chan struct{}
}

func (s *reviewR29RetryStore) CreateOrUpdateBridge(ctx context.Context, tenant string, b *store.Bridge) error {
	s.mu.Lock()
	fail := s.failNextBirth
	s.failNextBirth = false
	pause := s.pausePlain
	if pause {
		s.pausePlain = false
	}
	s.mu.Unlock()
	if fail {
		return errors.New("injected one birth write failure")
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

func TestReviewR29StaleRetryCannotUndoNewSealedBirth(t *testing.T) {
	ctx := context.Background()
	const imei = "300000000000029"
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
	s := &reviewR29RetryStore{Store: db, entered: make(chan struct{}), release: make(chan struct{})}
	b := &reviewR28Bus{}
	sub := bridge.NewSubscriber(b, s, nil)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Stop)
	if b.birth == nil {
		t.Fatal("birth subscription missing")
	}
	publish := func(format string) {
		birth := protocol.BridgeBirth{
			Protocol:  protocol.ProtocolVersion,
			BridgeID:  "review-kit",
			TenantID:  store.DefaultTenantID,
			Timestamp: time.Now(),
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
	publish("plain")
	s.mu.Lock()
	s.failNextBirth = true
	s.mu.Unlock()
	publish("plain")
	if _, format, held := sub.PendingEgress(store.DefaultTenantID, imei); !held || format != "plain" {
		t.Fatalf("premise: failed birth did not hold plain: %q %v", format, held)
	}
	s.mu.Lock()
	s.pausePlain = true
	s.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(s.release) }) }
	defer release()
	retryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		sub.ReconcileEgress(retryCtx)
		close(done)
	}()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not reach its pending map write")
	}
	publish("sealed")
	fresh := bridge.NewEgressDeclarations(db)
	if got := fresh.Declaration(ctx, store.DefaultTenantID, imei); got != codec.DeclaredSealed {
		t.Fatalf("premise: newer sealed birth did not persist: %v", got)
	}
	if _, _, held := sub.PendingEgress(store.DefaultTenantID, imei); held {
		t.Fatal("premise: successful newer birth left a hold")
	}
	t.Log("newer sealed birth committed; now release the older plain retry")
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not finish")
	}
	row, err := db.GetBridge(ctx, store.DefaultTenantID, "review-kit")
	if err != nil {
		t.Fatal(err)
	}
	var stored protocol.BridgeBirth
	if err := json.Unmarshal([]byte(row.LastBirth), &stored); err != nil {
		t.Fatal(err)
	}
	mapFormat, err := db.BridgeEgress(ctx, store.DefaultTenantID, "review-kit", imei)
	if err != nil {
		t.Fatal(err)
	}
	_, recordFormat, err := db.DeviceEgress(ctx, store.DefaultTenantID, imei)
	if err != nil {
		t.Fatal(err)
	}
	_, _, held := sub.PendingEgress(store.DefaultTenantID, imei)
	wired := bridge.NewEgressDeclarations(s)
	wired.SetPending(sub)
	t.Logf("birth=%s map=%s record=%s pending=%v wired=%s fresh=%s", stored.Interfaces[0].EgressFormat, mapFormat, recordFormat, held, wired.Declaration(ctx, store.DefaultTenantID, imei), fresh.Declaration(ctx, store.DefaultTenantID, imei))
	h := NewWebhookHandler(b)
	h.SetEgressDeclarations(wired)
	mo := rnsMO(imei, IMTTopicRaw, reviewR27Ciphertext(t), "00000000-0000-4000-8000-000000000029")
	h.ProcessLingoMO(ctx, mo)
	var decoded WebhookMOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("after_stale_retry opaque=%v compressed=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Compressed, decoded.Text, decoded.Wire == mo.Message)
	if mapFormat != "sealed" || !decoded.Opaque || decoded.Text != "" {
		t.Error("an older retry undid an already committed sealed declaration")
	}
	publish("sealed")
	mo.ID = "00000000-0000-4000-8000-000000000030"
	h.ProcessLingoMO(ctx, mo)
	decoded = WebhookMOMessage{}
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("after_new_sealed_replay opaque=%v text=%q", decoded.Opaque, decoded.Text)
	if !decoded.Opaque || decoded.Text != "" {
		t.Fatal("new sealed replay did not restore the control")
	}
}

func TestReviewR29FailedFirstBirthMustNotLoseItsDeclarationOnRetry(t *testing.T) {
	for _, knownBridge := range []bool{false, true} {
		name := "new bridge"
		if knownBridge {
			name = "existing bridge awaiting modem identity"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			const imei = "300000000000031"
			db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: "new kit modem"}); err != nil {
				t.Fatal(err)
			}
			s := &reviewR29RetryStore{Store: db}
			b := &reviewR28Bus{}
			sub := bridge.NewSubscriber(b, s, nil)
			if err := sub.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sub.Stop)
			if b.birth == nil {
				t.Fatal("birth subscription missing")
			}
			birth := protocol.BridgeBirth{
				Protocol:  protocol.ProtocolVersion,
				BridgeID:  "review-new-kit",
				TenantID:  store.DefaultTenantID,
				Timestamp: time.Now(),
				Interfaces: []protocol.InterfaceInfo{{
					Name: "iridium_imt", Type: "iridium_imt", IMEI: imei,
					EgressFormat: "sealed", EgressSealed: true,
				}},
			}
			data, err := json.Marshal(birth)
			if err != nil {
				t.Fatal(err)
			}
			if knownBridge {
				initial := birth
				initial.Interfaces = nil
				initialData, err := json.Marshal(initial)
				if err != nil {
					t.Fatal(err)
				}
				b.birth("meshsat/bridge/review-new-kit/birth", initialData)
			}
			s.mu.Lock()
			s.failNextBirth = true
			s.mu.Unlock()
			b.birth("meshsat/bridge/review-new-kit/birth", data)
			wired := bridge.NewEgressDeclarations(s)
			wired.SetPending(sub)
			if got := wired.Declaration(ctx, store.DefaultTenantID, imei); got != codec.DeclaredSealed {
				t.Fatalf("premise: the failed first birth was not held sealed: %v", got)
			}
			_, _, pendingBefore := sub.PendingEgress(store.DefaultTenantID, imei)
			sub.ReconcileEgress(ctx)
			owner, err := db.DeviceBridgeID(ctx, store.DefaultTenantID, imei)
			if err != nil {
				t.Fatal(err)
			}
			_, record, err := db.DeviceEgress(ctx, store.DefaultTenantID, imei)
			if err != nil {
				t.Fatal(err)
			}
			row, bridgeErr := db.GetBridge(ctx, store.DefaultTenantID, "review-new-kit")
			_, _, pendingAfter := sub.PendingEgress(store.DefaultTenantID, imei)
			fresh := bridge.NewEgressDeclarations(db)
			t.Logf("pending_before=%v pending_after=%v bridge_exists=%v bridge_error=%v owner=%q record=%s wired=%s fresh=%s", pendingBefore, pendingAfter, row != nil, bridgeErr, owner, record, wired.Declaration(ctx, store.DefaultTenantID, imei), fresh.Declaration(ctx, store.DefaultTenantID, imei))
			h := NewWebhookHandler(b)
			h.SetEgressDeclarations(wired)
			mo := rnsMO(imei, IMTTopicRaw, reviewR27Ciphertext(t), "00000000-0000-4000-8000-000000000031")
			h.ProcessLingoMO(ctx, mo)
			var decoded WebhookMOMessage
			if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
				t.Fatal(err)
			}
			t.Logf("after_first_birth_retry opaque=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Text, decoded.Wire == mo.Message)
			if !decoded.Opaque || decoded.Text != "" {
				t.Error("retry discarded a sealed declaration without establishing its bridge and modem ownership")
			}
			b.birth("meshsat/bridge/review-new-kit/birth", data)
			mo.ID = "00000000-0000-4000-8000-000000000032"
			h.ProcessLingoMO(ctx, mo)
			decoded = WebhookMOMessage{}
			if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
				t.Fatal(err)
			}
			t.Logf("after_successful_first_birth_replay opaque=%v text=%q", decoded.Opaque, decoded.Text)
			if !decoded.Opaque || decoded.Text != "" {
				t.Fatal("successful birth replay did not restore the control")
			}
		})
	}
}
