package cloudloop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

type reviewR28Bus struct {
	ackBus
	birth bus.MessageHandler
}

func (b *reviewR28Bus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	if topic == protocol.SubBridgeBirth {
		b.birth = h
	}
	return nil
}

type reviewR28WriteStore struct {
	store.Store
	fail     bool
	failures int
}

func (s *reviewR28WriteStore) SetDeviceEgress(ctx context.Context, tenant, imei, owner, format string) error {
	if s.fail {
		s.failures++
		return errors.New("injected temporary declaration-write failure")
	}
	return s.Store.SetDeviceEgress(ctx, tenant, imei, owner, format)
}

func TestReviewR28FailedDeclarationWriteCannotKeepPlain(t *testing.T) {
	ctx := context.Background()
	const imei = "300000000000091"
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
	s := &reviewR28WriteStore{Store: db}
	b := &reviewR28Bus{}
	sub := bridge.NewSubscriber(b, s, nil)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
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
	d := bridge.NewEgressDeclarations(s)
	if got := d.Declaration(ctx, store.DefaultTenantID, imei); got != codec.DeclaredPlain {
		t.Fatalf("initial plain profile was not installed: %v", got)
	}
	s.fail = true
	publish("sealed")
	s.fail = false
	if s.failures != 1 {
		t.Fatalf("injected %d write failures, want one", s.failures)
	}
	row, err := db.GetBridge(ctx, store.DefaultTenantID, "review-kit")
	if err != nil {
		t.Fatal(err)
	}
	var stored protocol.BridgeBirth
	if err := json.Unmarshal([]byte(row.LastBirth), &stored); err != nil {
		t.Fatal(err)
	}
	_, record, err := db.DeviceEgress(ctx, store.DefaultTenantID, imei)
	if err != nil {
		t.Fatal(err)
	}
	fresh := bridge.NewEgressDeclarations(db)
	t.Logf("new_birth=%s durable_record=%s existing_reader=%s fresh_reader=%s", stored.Interfaces[0].EgressFormat, record, d.Declaration(ctx, store.DefaultTenantID, imei), fresh.Declaration(ctx, store.DefaultTenantID, imei))
	h := NewWebhookHandler(b)
	h.SetEgressDeclarations(fresh)
	wire := reviewR27Ciphertext(t)
	mo := rnsMO(imei, IMTTopicRaw, wire, "00000000-0000-4000-8000-000000000091")
	h.ProcessLingoMO(ctx, mo)
	var decoded WebhookMOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("after_write_failure opaque=%v compressed=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Compressed, decoded.Text, decoded.Wire == mo.Message)
	if !decoded.Opaque || decoded.Text != "" {
		t.Error("a failed declaration write left the previous plain profile authoritative for new ciphertext")
	}
	publish("sealed")
	mo.ID = "00000000-0000-4000-8000-000000000092"
	h.ProcessLingoMO(ctx, mo)
	decoded = WebhookMOMessage{}
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("after_successful_replay opaque=%v text=%q", decoded.Opaque, decoded.Text)
	if !decoded.Opaque || decoded.Text != "" {
		t.Fatal("successful declaration replay did not restore the control")
	}
}
