package warnings

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1734: kits only. A phone registered as a bridge declares a
// "meshtastic" interface (its BLE node) like a kit does, so the rule has to
// read the birth's mode, not only its interfaces.

// birthJSON is a stored birth of the given mode declaring one interface per
// type, named the way the Bridge names them.
func birthJSON(mode string, types ...string) string {
	b := protocol.BridgeBirth{Protocol: "1.0", Mode: mode, Interfaces: []protocol.InterfaceInfo{}}
	for _, t := range types {
		b.Interfaces = append(b.Interfaces, protocol.InterfaceInfo{Name: t + "_0", Type: t, Status: "online"})
	}
	j, _ := json.Marshal(b)
	return string(j)
}

func TestCanRelay(t *testing.T) {
	cases := []struct {
		name   string
		bridge *store.Bridge
		ok     bool
		why    string
	}{
		{"a kit with a mesh", &store.Bridge{BridgeID: "a", LastBirth: birthJSON("direct", "meshtastic", "iridium", "cellular")}, true, ""},
		{"a kit with APRS only", &store.Bridge{BridgeID: "b", LastBirth: birthJSON("direct", "aprs")}, true, ""},
		{"a kit running the 2G cell only", &store.Bridge{BridgeID: "c", LastBirth: birthJSON("cubeos", "cell")}, true, ""},
		{"an interface declared offline still counts", &store.Bridge{BridgeID: "d", LastBirth: strings.ReplaceAll(birthJSON("direct", "meshtastic"), "online", "offline")}, true, ""},
		{"an Android phone with a BLE mesh", &store.Bridge{BridgeID: "msa-x", LastBirth: birthJSON("android", "meshtastic", "iridium_sbd")}, false, "a phone"},
		{"an iPhone", &store.Bridge{BridgeID: "msi-x", LastBirth: birthJSON("ios", "meshtastic")}, false, "a phone"},
		{"a kit with a satellite modem and nothing to relay on", &store.Bridge{BridgeID: "e", LastBirth: birthJSON("direct", "iridium_imt", "mqtt")}, false, "no mesh, APRS or 2G cell interface"},
		{"a bridge that never connected", &store.Bridge{BridgeID: "f"}, false, "no birth yet"},
		{"an unreadable birth", &store.Bridge{BridgeID: "g", LastBirth: "{"}, false, "unreadable birth"},
		{"nil", nil, false, "no bridge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, why := CanRelay(c.bridge)
			if ok != c.ok || why != c.why {
				t.Fatalf("ok=%v why=%q, want ok=%v why=%q", ok, why, c.ok, c.why)
			}
		})
	}
}

func TestOnlyKitsAreHandedADigest(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	// NL804 sends to every kit of the tenant; the phone sits on Texel too.
	svc, bus, db, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804"}},
		&fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}})
	ctx := context.Background()
	// Two more bridges of t1: one that never sent a birth, one whose birth
	// declares only a satellite modem.
	for _, b := range []*store.Bridge{
		{BridgeID: "kit-new", LocationLat: 53.05, LocationLon: 4.80},
		{BridgeID: "kit-sat", LocationLat: 53.05, LocationLon: 4.80, LastBirth: birthJSON("direct", "iridium_imt")},
	} {
		if err := db.CreateOrUpdateBridge(ctx, "t1", b); err != nil {
			t.Fatal(err)
		}
	}
	svc.Poll(ctx)
	var topics []string
	for _, s := range bus.published() {
		topics = append(topics, s.topic)
	}
	want := []string{"meshsat/t1/bridge/leiden/warn/in", "meshsat/t1/bridge/texel/warn/in"}
	if !reflect.DeepEqual(topics, want) {
		t.Fatalf("published to %v, want the two kits %v", topics, want)
	}
	list, err := db.ListTenantWarnings(ctx, "t1", 10)
	if err != nil || len(list) != 1 || list[0].Bridges != `["leiden","texel"]` {
		t.Fatalf("recorded: %+v %v", list, err)
	}

	// The injection answer says the same and names the ones left out.
	res, err := svc.Inject(ctx, "t1", []byte(strings.ReplaceAll(strings.Replace(testCAP, "%s", "kits", 1), "%s", "Test")))
	if err != nil || len(res.Deliveries) != 1 {
		t.Fatalf("inject: %+v %v", res, err)
	}
	d := res.Deliveries[0]
	if !reflect.DeepEqual(d.Bridges, []string{"leiden", "texel"}) {
		t.Fatalf("bridges: %v", d.Bridges)
	}
	if !reflect.DeepEqual(d.Skipped, []string{"kit-new: no birth yet", "kit-sat: no mesh, APRS or 2G cell interface", "msa-flaneur: a phone"}) {
		t.Fatalf("skipped: %v", d.Skipped)
	}
	for _, s := range bus.published() {
		if strings.Contains(s.topic, "msa-flaneur") || strings.Contains(s.topic, "kit-new") || strings.Contains(s.topic, "kit-sat") {
			t.Fatalf("a digest went to %s", s.topic)
		}
	}

	// A workspace with a phone alone is told why nothing went out.
	if err := db.CreateTenant(ctx, &store.Tenant{ID: "t2", Slug: "t2", Name: "t2", Status: store.TenantActive}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrUpdateBridge(ctx, "t2", &store.Bridge{BridgeID: "msi-only", LastBirth: birthJSON("ios", "meshtastic")}); err != nil {
		t.Fatal(err)
	}
	svc.accounts = fakeAccounts{"t2": {"enabled": "true", "areas": "NL804"}}
	res, err = svc.Inject(ctx, "t2", []byte(strings.ReplaceAll(strings.Replace(testCAP, "%s", "phone", 1), "%s", "Test")))
	if err != nil || len(res.Deliveries) != 1 || len(res.Deliveries[0].Bridges) != 0 ||
		!strings.Contains(res.Deliveries[0].Error, "no kit of this workspace can relay") || !strings.Contains(res.Deliveries[0].Error, "msi-only: a phone") {
		t.Fatalf("phone-only workspace: %+v %v", res, err)
	}
}
