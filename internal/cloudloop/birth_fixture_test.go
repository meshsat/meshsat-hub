package cloudloop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The births a Bridge published in its stamp fault run (meshsat:
// TestBirthWithheldUntilItsStampIsStored, MESHSAT_BIRTH_FIXTURE_OUT), fed as
// published, in that order, to a Hub on a real database: the declaration the
// Hub keeps must be the last one the kit made, and a sealed one keeps the
// modem's ciphertext opaque. Skipped without MESHSAT_BIRTH_FIXTURE_IN.
// [MESHSAT-1584]
func TestBirthStampFaultRunOnTheHub(t *testing.T) {
	in := os.Getenv("MESHSAT_BIRTH_FIXTURE_IN")
	if in == "" {
		t.Skip("MESHSAT_BIRTH_FIXTURE_IN not set")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := &reviewR28Bus{}
	sub := bridge.NewSubscriber(b, db, nil)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Stop)
	var last protocol.BridgeBirth
	for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if err := json.Unmarshal(line, &last); err != nil {
			t.Fatal(err)
		}
		imei := last.Interfaces[0].IMEI
		if i == 0 {
			if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: "kit modem"}); err != nil {
				t.Fatal(err)
			}
		}
		b.birth("meshsat/bridge/"+last.BridgeID+"/birth", line)
		e, err := db.BridgeEgressEntry(ctx, store.DefaultTenantID, last.BridgeID, imei)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("birth %d: declares %s stamped %s; the Hub keeps %s", i, last.Interfaces[0].EgressFormat, last.Timestamp.UTC().Format("15:04:05.000000"), e.Format)
	}
	imei := last.Interfaces[0].IMEI
	want := last.Interfaces[0].EgressFormat
	if e, _ := db.BridgeEgressEntry(ctx, store.DefaultTenantID, last.BridgeID, imei); e.Format != want {
		t.Errorf("the Hub keeps %s, the kit last declared %s", e.Format, want)
	}
	if want != "sealed" {
		return
	}
	h := NewWebhookHandler(b)
	d := bridge.NewEgressDeclarations(db)
	d.SetPending(sub)
	h.SetEgressDeclarations(d)
	mo := rnsMO(imei, IMTTopicRaw, reviewR27Ciphertext(t), "00000000-0000-4000-8000-000000000051")
	h.ProcessLingoMO(ctx, mo)
	var decoded WebhookMOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("the kit's ciphertext: opaque=%v compressed=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Compressed, decoded.Text, decoded.Wire == mo.Message)
	if !decoded.Opaque || decoded.Text != "" {
		t.Error("the kit's sealed declaration did not keep its ciphertext opaque")
	}
}
