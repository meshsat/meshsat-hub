package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The r32 review's characterization test, as received: a phone-owned modem
// whose birth declares no format reads as undeclared, so the H-chain
// ciphertext takes the plain compatibility path; once its birth declares
// sealed, the same payload stays opaque. The phone apps (MESHSAT-1623,
// MESHSAT-1624) must reach the sealed branch (R32-R1).
func TestReviewR32PhoneModemNeedsSealedDeclaration(t *testing.T) {
	ctx := context.Background()
	const phone = "review-phone"
	const imei = "300000000000032"
	db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: "synthetic phone modem"}); err != nil {
		t.Fatal(err)
	}
	b := &reviewR28Bus{}
	sub := bridge.NewSubscriber(b, db, nil)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Stop)
	d := bridge.NewEgressDeclarations(db)
	d.SetPending(sub)
	h := NewWebhookHandler(b)
	h.SetEgressDeclarations(d)
	wire := reviewR27Ciphertext(t)
	start := time.Now().UTC()
	for i, format := range []string{"", "sealed"} {
		birth := protocol.BridgeBirth{
			Protocol:  protocol.ProtocolVersion,
			BridgeID:  phone,
			TenantID:  store.DefaultTenantID,
			Timestamp: start.Add(time.Duration(i) * time.Second),
			Interfaces: []protocol.InterfaceInfo{{
				Name: "iridium_imt", Type: "iridium_imt", Status: "online",
				IMEI: imei, EgressFormat: format,
			}},
		}
		raw, err := json.Marshal(birth)
		if err != nil {
			t.Fatal(err)
		}
		b.birth("meshsat/bridge/"+phone+"/birth", raw)
		owner, err := db.DeviceBridgeID(ctx, store.DefaultTenantID, imei)
		if err != nil || owner != phone {
			t.Fatalf("owner=%q err=%v", owner, err)
		}
		decl := d.Declaration(ctx, store.DefaultTenantID, imei)
		mo := rnsMO(imei, IMTTopicRaw, wire, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
		h.ProcessLingoMO(ctx, mo)
		var got WebhookMOMessage
		if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &got); err != nil {
			t.Fatal(err)
		}
		t.Logf("declaration=%s opaque=%v compressed=%v text=%q wire_preserved=%v", decl.String(), got.Opaque, got.Compressed, got.Text, got.Wire == mo.Message)
		if got.Wire != mo.Message {
			t.Fatal("wire changed")
		}
		if i == 0 {
			if decl != codec.Undeclared || got.Opaque || got.Text == "" {
				t.Fatal("the undeclared control did not reproduce the expected compatibility-path hazard")
			}
		} else if decl != codec.DeclaredSealed || !got.Opaque || got.Text != "" {
			t.Fatal("a phone-owned modem's sealed declaration did not keep its ciphertext opaque")
		}
	}
}
