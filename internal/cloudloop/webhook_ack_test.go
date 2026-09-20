package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

type ackBus struct {
	mu   sync.Mutex
	msgs map[string][]byte
}

func (b *ackBus) Connect() error                                                { return nil }
func (b *ackBus) Disconnect()                                                   {}
func (b *ackBus) IsConnected() bool                                             { return true }
func (b *ackBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *ackBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (b *ackBus) Publish(topic string, _ byte, _ bool, p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.msgs == nil {
		b.msgs = map[string][]byte{}
	}
	b.msgs[topic] = p
	return nil
}
func (b *ackBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	return b.Publish(topic, qos, retained, data)
}
func (b *ackBus) topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.msgs))
	for k := range b.msgs {
		out = append(out, k)
	}
	return out
}

// The Cloudloop half of the MO receipt (MESHSAT-1257): it goes to the bridge
// that owns the modem, on that bridge's own topic, carrying the MOMSN the
// modem reported for the session. A modem no bridge owns gets none, and an IMT
// delivery gets none either -- see the IMT case below for why that is the point
// rather than an omission.
func TestWebhook_MOReceiptGoesToTheOwningBridge(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const owned, loose, imtIMEI = "300234065000001", "300234065000077", "300234065000099"
	for _, imei := range []string{owned, loose, imtIMEI} {
		if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: imei}); err != nil {
			t.Fatal(err)
		}
	}
	for _, imei := range []string{owned, imtIMEI} {
		if err := db.AssociateDeviceWithBridge(ctx, store.DefaultTenantID, imei, "msa-flaneur"); err != nil {
			t.Fatal(err)
		}
	}

	b := &ackBus{}
	h := NewWebhookHandler(b)
	h.SetStore(db)

	sbdMO := func(imei string, momsn int) *LingoMO {
		return &LingoMO{
			ID:       "mo-" + imei,
			Identity: LingoIdentity{ThingID: "thing-" + imei, Hardware: &LingoHardware{IMEI: imei, Type: "HARDWARE_TYPE_IRIDIUM_SBD"}},
			SBD:      &LingoSBD{IMEI: imei, MOMSN: momsn},
			Message:  base64.StdEncoding.EncodeToString([]byte("hi")),
		}
	}

	t.Run("the owning bridge gets the receipt with the session MOMSN", func(t *testing.T) {
		h.ProcessLingoMO(ctx, sbdMO(owned, 223))
		raw, ok := b.msgs["meshsat/bridge/msa-flaneur/mo/ack"]
		if !ok {
			t.Fatalf("no receipt on the owning bridge's topic; published: %v", b.topics())
		}
		var ack hubmqtt.MOAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.IMEI != owned {
			t.Errorf("IMEI = %q, want %q", ack.IMEI, owned)
		}
		if ack.MOMSN != 223 {
			t.Errorf("MOMSN = %d, want 223 -- the receipt must carry the number the modem reported", ack.MOMSN)
		}
		if ack.Bearer != "sbd" {
			t.Errorf("Bearer = %q, want sbd", ack.Bearer)
		}
		if ack.ReceivedAt == "" {
			t.Error("ReceivedAt empty")
		}
	})

	t.Run("a modem no bridge owns gets no receipt", func(t *testing.T) {
		b.mu.Lock()
		b.msgs = nil
		b.mu.Unlock()
		h.ProcessLingoMO(ctx, sbdMO(loose, 5))
		for _, topic := range b.topics() {
			if topic == "meshsat/bridge/msa-flaneur/mo/ack" {
				t.Fatal("a modem with no bridge produced a receipt on another bridge's topic")
			}
		}
	})

	// An IMT delivery has no MOMSN: the 9704 reports cmid and messageId, which
	// the modem never saw, and LingoMO.MOMSN() answers 0 for anything that is
	// not SBD. Zero is a REAL momsn -- a modem's first session -- so a receipt
	// carrying it could tick the WRONG message rather than none. The Android
	// sender agrees by construction: it only records a satellite reference in
	// the 9603 branch.
	t.Run("an IMT delivery gets no receipt rather than a zero MOMSN", func(t *testing.T) {
		b.mu.Lock()
		b.msgs = nil
		b.mu.Unlock()
		h.ProcessLingoMO(ctx, &LingoMO{
			ID:       "mo-imt",
			Identity: LingoIdentity{ThingID: "thing-imt", Hardware: &LingoHardware{IMEI: imtIMEI, Type: "HARDWARE_TYPE_IRIDIUM_IMT"}},
			IMT:      &LingoIMT{CMID: "cm-1", Topic: "IMT_TOPIC_PURPLE"},
			Message:  base64.StdEncoding.EncodeToString([]byte("hi")),
		})
		if raw, ok := b.msgs["meshsat/bridge/msa-flaneur/mo/ack"]; ok {
			t.Fatalf("IMT produced a receipt, which cannot be correlated: %s", raw)
		}
	})
}
