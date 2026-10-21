package cloudloop

import (
	"bytes"
	"context"
	"encoding/hex"
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
//
// The births of MeshSat Android's live run (HubBirthDeclarationLiveTest,
// births.jsonl) go through the same way: the modem is the iridium_imt entry
// with an IMEI, a birth that names none is fed as published, and
// MESHSAT_BIRTH_FIXTURE_WIRE_HEX gives the phone's own sealed wire for the
// MO. [MESHSAT-1623]
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
	var modem protocol.InterfaceInfo
	created := false
	for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		last = protocol.BridgeBirth{}
		if err := json.Unmarshal(line, &last); err != nil {
			t.Fatal(err)
		}
		named := false
		for _, iface := range last.Interfaces {
			if iface.IMEI != "" {
				modem, named = iface, true
				break
			}
		}
		if named && !created {
			if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: modem.IMEI, Label: "kit modem"}); err != nil {
				t.Fatal(err)
			}
			created = true
		}
		b.birth("meshsat/bridge/"+last.BridgeID+"/birth", line)
		if !created {
			t.Logf("birth %d: names no modem, stamped %s", i, last.Timestamp.UTC().Format("15:04:05.000000"))
			continue
		}
		e, err := db.BridgeEgressEntry(ctx, store.DefaultTenantID, last.BridgeID, modem.IMEI)
		if err != nil {
			t.Fatal(err)
		}
		declares := "nothing"
		if named && modem.EgressFormat != "" {
			declares = modem.EgressFormat
		}
		t.Logf("birth %d: declares %s stamped %s; the Hub keeps %q", i, declares, last.Timestamp.UTC().Format("15:04:05.000000"), e.Format)
	}
	imei := modem.IMEI
	// What the Hub must keep: the last declaration made, not the last birth (one that
	// declares nothing leaves it as it was).
	want := ""
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var bb protocol.BridgeBirth
		_ = json.Unmarshal(line, &bb)
		for _, iface := range bb.Interfaces {
			if iface.IMEI == imei && iface.EgressFormat != "" {
				want = iface.EgressFormat
			}
		}
	}
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
	wire := reviewR27Ciphertext(t)
	if h := os.Getenv("MESHSAT_BIRTH_FIXTURE_WIRE_HEX"); h != "" {
		if wire, err = hex.DecodeString(h); err != nil {
			t.Fatal(err)
		}
	}
	mo := rnsMO(imei, IMTTopicRaw, wire, "00000000-0000-4000-8000-000000000051")
	h.ProcessLingoMO(ctx, mo)
	var decoded WebhookMOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &decoded); err != nil {
		t.Fatal(err)
	}
	t.Logf("the modem's ciphertext: opaque=%v compressed=%v text=%q wire_preserved=%v", decoded.Opaque, decoded.Compressed, decoded.Text, decoded.Wire == mo.Message)
	if !decoded.Opaque || decoded.Text != "" {
		t.Error("the sealed declaration did not keep the modem's ciphertext opaque")
	}
}
