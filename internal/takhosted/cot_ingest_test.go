package takhosted

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// MESHSAT-1458: what kits and apps export to the Hub reaches their own tenant's
// TAK servers, and nothing else does.
//
// ⚠ The victim in every isolation test is store.DefaultTenantID, deliberately,
// per the house rule: a bug with no tenant dimension collapses onto the default
// tenant, and a test between two non-default tenants would pass by accident.

// --- fixtures ----------------------------------------------------------------

// storeWithBridges registers the given bridge ids under their tenants.
func storeWithBridges(t *testing.T, bridges map[string][]string) store.Store {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for tenant, ids := range bridges {
		// The default tenant exists after a migration; creating it again fails,
		// and that failure is not this fixture's business.
		_ = db.CreateTenant(ctx, &store.Tenant{ID: tenant, Slug: tenant, Name: tenant, Status: store.TenantActive})
		for _, id := range ids {
			if err := db.CreateOrUpdateBridge(ctx, tenant, &store.Bridge{BridgeID: id, Label: id}); err != nil {
				t.Fatalf("register bridge %s in %s: %v", id, tenant, err)
			}
			if b, err := db.GetBridge(ctx, tenant, id); err != nil || b == nil {
				t.Fatalf("bridge %s is not readable back from %s: %v", id, tenant, err)
			}
		}
	}
	return db
}

// cotEvent is an event as a kit or an app exports it, current as of now.
func cotEvent(uid, typ, detail string) []byte {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	stale := now.Add(2 * time.Minute).Format(time.RFC3339)
	return []byte(`<event version="2.0" uid="` + uid + `" type="` + typ + `" how="m-g" time="` + ts +
		`" start="` + ts + `" stale="` + stale + `"><point lat="52.1" lon="4.5" hae="0" ce="10" le="10"/>` +
		`<detail>` + detail + `</detail></event>`)
}

func cotPLI(uid string) []byte {
	return cotEvent(uid, "a-f-G-U-C", `<contact callsign="KIT-1A2B"/><__group name="Cyan" role="Team Member"/>`)
}

// export publishes a payload on a bridge's export topic and requires that
// something is subscribed to it.
func export(t *testing.T, b *fakeBus, tenant, bridge string, payload []byte) {
	t.Helper()
	topic := hubmqtt.TopicTAKCotOutFor(tenant, bridge)
	if n := b.deliver(topic, payload); n == 0 {
		t.Fatalf("nothing is subscribed to %s", topic)
	}
}

// cotCount reads one result of the exported-CoT counter.
func cotCount(t *testing.T, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "meshsat_hub_takhosted_cot_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var src, res string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "source":
					src = l.GetValue()
				case "result":
					res = l.GetValue()
				}
			}
			if src == sourceClient && res == result {
				return m.GetCounter().GetValue()
			}
		}
		t.Fatalf("no series for result=%q: it is not materialised, so a rate alert on it could never fire", result)
	}
	t.Fatal("the counter meshsat_hub_takhosted_cot_total is not registered")
	return 0
}

// waitCount waits for a counter to have moved by at least want.
func waitCount(t *testing.T, result string, before, want float64) float64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for cotCount(t, result)-before < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return cotCount(t, result) - before
}

// oneTenantWithBridge is the common case: tenant t1, bridge kit1, one server.
func oneTenantWithBridge(t *testing.T) (*capturedOTS, *fakeBus, *Forwarder) {
	t.Helper()
	ots := newCapturedOTS(t)
	bf := forwarderForTenantsF(t, storeWithBridges(t, map[string][]string{"t1": {"kit1"}}),
		map[string]*capturedOTS{"t1": ots})
	return ots, bf.fakeBus, bf.f
}

// stillForwards is the positive control beside an assertion that something was
// NOT forwarded: without it such an assertion passes on a forwarder that is
// simply not wired.
func stillForwards(t *testing.T, ots *capturedOTS, b *fakeBus, tenant, bridge string, already int) {
	t.Helper()
	const control = "MESHSAT-c0ffee00"
	export(t, b, tenant, bridge, cotPLI(control))
	got := ots.waitFor(t, already+1)
	if len(got) != already+1 {
		t.Fatalf("the positive control did not arrive (%d events, want %d): the forwarder is not "+
			"wired for exported CoT, so the assertion above proved nothing", len(got), already+1)
	}
	if !strings.Contains(got[len(got)-1], control) {
		t.Fatalf("the last event is not the control:\n%s", got[len(got)-1])
	}
}

// --- it arrives --------------------------------------------------------------

func TestAClientsCoTReachesTheTenantsTAKServer(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotForwarded)

	export(t, b, "t1", "kit1", cotPLI("MESHSAT-aabbccdd"))

	got := ots.waitFor(t, 1)
	if len(got) != 1 {
		t.Fatalf("%d events reached the tenant's TAK server, want 1", len(got))
	}
	for _, want := range []string{`uid="MESHSAT-aabbccdd"`, `callsign="KIT-1A2B"`, `<__group name="Cyan" role="Team Member"/>`} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the event lost %s on the way through:\n%s", want, got[0])
		}
	}
	if moved := waitCount(t, cotForwarded, before, 1); moved != 1 {
		t.Errorf("the forwarded counter moved by %v, want 1", moved)
	}
}

// meshsat/bridge/+/... does not match meshsat/{tenant}/bridge/+/..., so subscribing
// to one shape leaves either the default tenant or every other tenant silent.
func TestBothTopicShapesOfExportedCoTAreForwarded(t *testing.T) {
	platform, customer := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-p"}, "t-cust": {"kit-c"}}),
		map[string]*capturedOTS{store.DefaultTenantID: platform, "t-cust": customer})

	legacy := "meshsat/bridge/kit-p/tak/cot/out"
	if got := hubmqtt.TopicTAKCotOutFor(store.DefaultTenantID, "kit-p"); got != legacy {
		t.Fatalf("the default tenant's export topic is %q, want the legacy shape %q", got, legacy)
	}
	export(t, bf.fakeBus, store.DefaultTenantID, "kit-p", cotPLI("MESHSAT-00000001"))
	export(t, bf.fakeBus, "t-cust", "kit-c", cotPLI("MESHSAT-00000002"))

	if got := platform.waitFor(t, 1); len(got) != 1 || !strings.Contains(got[0], "MESHSAT-00000001") {
		t.Errorf("the legacy shape did not reach the default tenant's server: %v", got)
	}
	if got := customer.waitFor(t, 1); len(got) != 1 || !strings.Contains(got[0], "MESHSAT-00000002") {
		t.Errorf("the tenant shape did not reach the customer's server: %v", got)
	}
}

// --- it arrives nowhere else -------------------------------------------------

func TestOneTenantsExportedCoTNeverReachesAnotherTenantsTAKServer(t *testing.T) {
	victim, other := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-other": {"kit-o"}}),
		map[string]*capturedOTS{store.DefaultTenantID: victim, "t-other": other})

	export(t, bf.fakeBus, "t-other", "kit-o", cotPLI("MESHSAT-0000beef"))

	if got := other.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the customer's own server received %d events, want 1", len(got))
	}
	quiet(t, victim, "the platform's TAK server", 300*time.Millisecond)
	stillForwards(t, victim, bf.fakeBus, store.DefaultTenantID, "kit-v", 0)
}

func TestThePlatformsExportedCoTNeverReachesACustomersTAKServer(t *testing.T) {
	platform, customer := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-p"}, "t-cust": {"kit-c"}}),
		map[string]*capturedOTS{store.DefaultTenantID: platform, "t-cust": customer})

	export(t, bf.fakeBus, store.DefaultTenantID, "kit-p", cotPLI("MESHSAT-0000beef"))

	if got := platform.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the platform's own server received %d events, want 1", len(got))
	}
	quiet(t, customer, "a customer's TAK server", 300*time.Millisecond)
	stillForwards(t, customer, bf.fakeBus, "t-cust", "kit-c", 0)
}

// The tenant is the one the topic names, and the bridge must be registered to
// it. Resolving by store owner instead would file this event under the tenant
// that owns the bridge id: one tenant writing into another's map.
func TestABridgeOwnedByAnotherTenantIsRefusedNotAdopted(t *testing.T) {
	victim, attacker := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		// kit-v belongs to the default tenant. The attacker has a bridge of its
		// own, so its server is reachable and the control below can run.
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-attacker": {"kit-a"}}),
		map[string]*capturedOTS{store.DefaultTenantID: victim, "t-attacker": attacker})
	before := cotCount(t, cotUnregistered)

	// The attacker's namespace, the victim's bridge id.
	export(t, bf.fakeBus, "t-attacker", "kit-v", cotPLI("MESHSAT-0000dead"))
	// And the id a wide device grant can reach in any namespace: nobody
	// registered a bridge called "mo".
	export(t, bf.fakeBus, store.DefaultTenantID, "mo", cotPLI("MESHSAT-0000dead"))

	if moved := waitCount(t, cotUnregistered, before, 2); moved != 2 {
		t.Fatalf("%v events were counted as from an unregistered bridge, want 2", moved)
	}
	quiet(t, victim, "the tenant that owns the bridge id", 300*time.Millisecond)
	quiet(t, attacker, "the tenant whose namespace was used", 300*time.Millisecond)
	stillForwards(t, victim, bf.fakeBus, store.DefaultTenantID, "kit-v", 0)
	stillForwards(t, attacker, bf.fakeBus, "t-attacker", "kit-a", 0)
}

// The export lives on the bridge subtree. A device-shaped .../tak/cot/out is what
// the wide grants on device topics can reach in another tenant's namespace, so
// the Hub must not be listening there at all.
func TestExportedCoTOnADeviceShapedTopicIsNotConsumed(t *testing.T) {
	_, b, _ := oneTenantWithBridge(t)
	for _, topic := range []string{
		"meshsat/kit1/tak/cot/out",          // the shape the clients shipped with
		"meshsat/t1/kit1/tak/cot/out",       // its tenant shape
		"meshsat/t1/mo/tak/cot/out",         // reachable through n.*.mo.>
		"meshsat/t1/status/tak/cot/out",     // and through n.*.status.>
		"meshsat/bridge/kit1/tak/cot/in",    // the other direction
		"meshsat/bridge/kit1/x/tak/cot/out", // one segment too many
	} {
		if n := b.deliver(topic, cotPLI("MESHSAT-0000dead")); n != 0 {
			t.Errorf("%d handlers are subscribed to %s", n, topic)
		}
	}
}

// --- what a hostile or broken client can and cannot do -----------------------

// The stream to a TAK server is events end to end. Whatever is written into it
// must be exactly one of them, or the server reads the rest as traffic the Hub
// vouched for.
func TestAPayloadThatIsNotExactlyOneEventNeverDesyncsTheStream(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	one := string(cotPLI("MESHSAT-0000dead"))

	for name, bad := range map[string]string{
		"two events":         one + one,
		"two events, a line": one + "\n" + one,
		"trailing bytes":     one + "<event",
		"unterminated":       strings.TrimSuffix(one, "</event>"),
		"doctype":            `<!DOCTYPE event [<!ENTITY x "y">]>` + one,
		"comment":            strings.Replace(one, "<detail>", "<detail><!-- x -->", 1),
		"cdata":              strings.Replace(one, "<detail>", "<detail><remarks><![CDATA[</event><event>]]></remarks>", 1),
		"nested event":       strings.Replace(one, "<detail>", "<detail><event/>", 1),
		"oversize":           strings.Replace(one, "<detail>", "<detail><remarks>"+strings.Repeat("x", tak.ClientLimits.MaxBytes)+"</remarks>", 1),
		"the route's json":   `{"device_id":"300434","text":"hello"}`,
		"a protobuf frame":   "\xbf\x01\xbf\x0a\x12",
		"empty":              "",
	} {
		export(t, b, "t1", "kit1", []byte(bad))
		_ = name
	}
	quiet(t, ots, "the TAK server, after twelve malformed payloads", 400*time.Millisecond)

	stillForwards(t, ots, b, "t1", "kit1", 0)
	for _, line := range ots.got() {
		if !strings.HasPrefix(line, "<event ") || !strings.HasSuffix(line, "</event>") || strings.Count(line, "</event>") != 1 {
			t.Errorf("a line in the stream is not exactly one event:\n%s", line)
		}
	}
}

func TestRawNewlinesNeverReachTheTAKStream(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	pretty := strings.NewReplacer("><", ">\n  <").Replace(string(cotEvent("MESHSAT-0000cafe", "b-t-f",
		"<remarks>first line\nsecond line\r\nthird</remarks>")))

	export(t, b, "t1", "kit1", []byte(pretty))

	ots.waitFor(t, 1)
	// The capture splits on line breaks, as a line-framed reader would. One event
	// must therefore be one line.
	time.Sleep(200 * time.Millisecond)
	got := ots.got()
	if len(got) != 1 {
		t.Fatalf("one event arrived as %d lines: a raw line break was written into the stream\n%q", len(got), got)
	}
	if !strings.Contains(got[0], "first line&#xA;second line&#xA;third") {
		t.Errorf("the text's own line breaks were lost rather than escaped:\n%s", got[0])
	}
}

// A keepalive or a protocol offer belongs to a connection, and the connection is
// the Hub's. Forwarding a client's request to switch protocol would switch the
// Hub's stream to one it does not speak.
func TestKeepalivesAndProtocolNegotiationAreNotForwarded(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotControl)
	types := []string{"t-x-c-t", "t-x-c-t-r", "t-x-takp-v", "t-x-takp-q", "t-x-takp-r"}
	for i, typ := range types {
		export(t, b, "t1", "kit1", cotEvent(fmt.Sprintf("ctl-%d", i), typ, ""))
	}
	if moved := waitCount(t, cotControl, before, float64(len(types))); moved != float64(len(types)) {
		t.Fatalf("%v control events were counted, want %d", moved, len(types))
	}
	quiet(t, ots, "the TAK server", 300*time.Millisecond)
	stillForwards(t, ots, b, "t1", "kit1", 0)
}

// meshsat-device-... and meshsat-bridge-... are the Hub's own markers. A client
// that could speak with those UIDs could move the Hub's markers.
func TestHubMarkerUIDsFromAClientAreRefused(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotHubUID)
	export(t, b, "t1", "kit1", cotPLI("meshsat-device-300434"))
	export(t, b, "t1", "kit1", cotPLI("meshsat-bridge-kit1"))
	if moved := waitCount(t, cotHubUID, before, 2); moved != 2 {
		t.Fatalf("%v events were refused for carrying the Hub's own UID, want 2", moved)
	}
	quiet(t, ots, "the TAK server", 300*time.Millisecond)
	stillForwards(t, ots, b, "t1", "kit1", 0)
}

// The export is QoS 1, so the broker may deliver an event twice.
func TestADuplicateDeliveryIsForwardedOnce(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotDuplicate)
	same := cotPLI("MESHSAT-0000aaaa")

	export(t, b, "t1", "kit1", same)
	export(t, b, "t1", "kit1", same)
	export(t, b, "t1", "kit1", cotPLI("MESHSAT-0000bbbb"))

	if got := ots.waitFor(t, 2); len(got) != 2 {
		t.Fatalf("%d events arrived, want 2 (the repeat forwarded once, the other event once)", len(got))
	}
	time.Sleep(200 * time.Millisecond)
	if got := ots.got(); len(got) != 2 {
		t.Errorf("%d events arrived, want 2: a redelivery was forwarded again", len(got))
	}
	if moved := cotCount(t, cotDuplicate) - before; moved != 1 {
		t.Errorf("%v duplicates were counted, want 1", moved)
	}
}

// --- budgets -----------------------------------------------------------------

// One client cannot use the Hub to flood its tenant's TAK server, and what it
// sends over its budget does not take the place of another tenant's event.
func TestAFloodFromOneTenantDoesNotStarveThePlatform(t *testing.T) {
	victim, flooder := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-flood": {"kit-f"}}),
		map[string]*capturedOTS{store.DefaultTenantID: victim, "t-flood": flooder})
	before := cotCount(t, cotRateLimited)

	const n = 2000
	began := time.Now()
	for i := 0; i < n; i++ {
		export(t, bf.fakeBus, "t-flood", "kit-f", cotPLI(fmt.Sprintf("MESHSAT-%08x", i)))
	}
	took := time.Since(began)

	// The platform's event is taken while the flood is still fresh.
	export(t, bf.fakeBus, store.DefaultTenantID, "kit-v", cotPLI("MESHSAT-0000beef"))
	if got := victim.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the platform's event did not arrive behind another tenant's flood: %d events", len(got))
	}

	limited := cotCount(t, cotRateLimited) - before
	// What the sender's budget allows in the time the flood took, with room for
	// the clock: its burst plus its refill.
	allowed := senderBurst + senderRate*took.Seconds() + 5
	if float64(n)-limited > allowed {
		t.Errorf("%v of %d flood events got through; the sender's budget allows about %.0f",
			float64(n)-limited, n, allowed)
	}
	time.Sleep(300 * time.Millisecond)
	if got := len(flooder.got()); float64(got) > allowed {
		t.Errorf("the flooding tenant's own server received %d events, over the %.0f its budget allows", got, allowed)
	}
	if limited == 0 {
		t.Error("nothing was rate limited, so the assertions above passed on a forwarder with no budget at all")
	}
}

// An SOS is never dropped because the same kit has spent its budget on position
// updates.
func TestAnEmergencyIsNotStarvedByAPLIFlood(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotRateLimited)
	for i := 0; i < 500; i++ {
		export(t, b, "t1", "kit1", cotPLI(fmt.Sprintf("MESHSAT-%08x", i)))
	}
	if cotCount(t, cotRateLimited)-before == 0 {
		t.Fatal("the flood did not exhaust the sender's budget, so this proves nothing about what happens when it is exhausted")
	}

	// Spelled both ways an emergency is spelled.
	export(t, b, "t1", "kit1", cotEvent("MESHSAT-0000aaaa-SOS", "a-f-G-U-C",
		`<contact callsign="KIT-1A2B"/><emergency type="911 Alert">KIT-1A2B</emergency>`))
	export(t, b, "t1", "kit1", cotEvent("MESHSAT-0000aaaa-DEADMAN", "b-a-o-tbl", `<contact callsign="KIT-1A2B"/>`))

	deadline := time.Now().Add(3 * time.Second)
	found := func() (sos, deadman bool) {
		for _, line := range ots.got() {
			sos = sos || strings.Contains(line, "MESHSAT-0000aaaa-SOS")
			deadman = deadman || strings.Contains(line, "MESHSAT-0000aaaa-DEADMAN")
		}
		return
	}
	for time.Now().Before(deadline) {
		if s, d := found(); s && d {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, d := found(); !s || !d {
		t.Errorf("an emergency was dropped behind a flood of position updates from the same kit "+
			"(SOS arrived: %v, missed check-in arrived: %v)", s, d)
	}
}

// --- one node, one marker ----------------------------------------------------

// A kit reports a node's position AND exports that node's own marker. Forward
// both and one radio is two dots on every map.
func TestAKitThatExportsItsOwnPLIIsNotAlsoDrawnFromItsPosition(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)

	export(t, b, "t1", "kit1", cotPLI("MESHSAT-aabbccdd"))
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the node's own marker did not arrive: %d events", len(got))
	}

	// The same node, as the kit's position report names it.
	deliverPosition(t, b, "t1", "!aabbccdd", 52.1)
	// A different node the kit relays for and does not export: still drawn.
	deliverPosition(t, b, "t1", "!11223344", 52.2)

	ots.waitFor(t, 2)
	time.Sleep(300 * time.Millisecond)
	got := ots.got()
	if len(got) != 2 {
		t.Fatalf("%d events arrived, want 2 (the node's own marker and the other node's)\n%v", len(got), got)
	}
	all := strings.Join(got, "\n")
	if strings.Contains(all, "meshsat-device-!aabbccdd") {
		t.Errorf("a node that exports its own marker was also drawn by the Hub: it is on the map twice\n%s", all)
	}
	if !strings.Contains(all, "meshsat-device-!11223344") {
		t.Errorf("a node the kit does not export was taken off the map along with the one it does\n%s", all)
	}
}

// Only a position event marks a node. A chat message or a missed check-in from
// the same kit says nothing about where the node is drawn from.
func TestOnlyAPositionEventStopsTheHubDrawingANode(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	export(t, b, "t1", "kit1", cotEvent("MESHSAT-aabbccdd-CHAT-k1", "b-t-f", "<remarks>hello</remarks>"))
	ots.waitFor(t, 1)

	deliverPosition(t, b, "t1", "!aabbccdd", 52.1)
	got := ots.waitFor(t, 2)
	if len(got) != 2 || !strings.Contains(got[1], "meshsat-device-!aabbccdd") {
		t.Errorf("a chat message stopped the Hub drawing the node it came from: %v", got)
	}
}

func TestSuppressionNeverCrossesTenants(t *testing.T) {
	victim, other := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-other": {"kit-o"}}),
		map[string]*capturedOTS{store.DefaultTenantID: victim, "t-other": other})

	// Another tenant exports a marker for a node id the platform also has.
	export(t, bf.fakeBus, "t-other", "kit-o", cotPLI("MESHSAT-aabbccdd"))
	other.waitFor(t, 1)

	deliverPosition(t, bf.fakeBus, store.DefaultTenantID, "!aabbccdd", 52.1)
	if got := victim.waitFor(t, 1); len(got) != 1 || !strings.Contains(got[0], "meshsat-device-!aabbccdd") {
		t.Errorf("another tenant's export took the platform's own node off the platform's map: %v", got)
	}
}

// --- the pieces --------------------------------------------------------------

func TestNodeKeyReducesTheIDsOfOneNodeToOneKey(t *testing.T) {
	want := "aabbccdd"
	for _, id := range []string{"!aabbccdd", "MESHSAT-aabbccdd", "meshsat-aabbccdd", "meshsat-device-!aabbccdd", " MESHSAT-AABBCCDD "} {
		if got := nodeKey(id); got != want {
			t.Errorf("nodeKey(%q) = %q, want %q", id, got, want)
		}
	}
	// An iPhone: the position id is its bridge id and the UID is MESHSAT-<bridge id>.
	if nodeKey("ios-1a2b3c4d5e6f") != nodeKey("MESHSAT-ios-1a2b3c4d5e6f") {
		t.Error("an iPhone's position id and its exported UID are not the same node")
	}
	// The events a kit exports that are NOT the node's position must not collide
	// with the node.
	for _, id := range []string{"MESHSAT-aabbccdd-DEADMAN", "MESHSAT-aabbccdd-CHAT-k1", "meshsat-aabbccdd-SENSOR"} {
		if nodeKey(id) == want {
			t.Errorf("nodeKey(%q) is the node's own key", id)
		}
	}
}

func TestTheSuppressionHoldIsClamped(t *testing.T) {
	s := newSuppressor()
	now := time.Unix(1_760_000_000, 0)

	// Stale already: held for the minimum, so a kit whose events go stale faster
	// than it reports does not flicker between two markers.
	s.mark("t1", "MESHSAT-aa", now.Add(-time.Hour), now)
	if !s.suppressed("t1", "!aa", now.Add(suppressMin-time.Second)) {
		t.Error("released before the minimum hold")
	}
	if s.suppressed("t1", "!aa", now.Add(suppressMin+time.Second)) {
		t.Error("held past the minimum for an event that was already stale")
	}

	// A far-off stale time cannot take a node off the Hub's own drawing for good.
	s.mark("t1", "MESHSAT-bb", now.Add(365*24*time.Hour), now)
	if s.suppressed("t1", "!bb", now.Add(suppressMax+time.Second)) {
		t.Error("held past the maximum: one event with a distant stale time would hide a node indefinitely")
	}

	s.prune(now.Add(suppressMax + time.Minute))
	if len(s.until) != 0 {
		t.Errorf("%d marks left after they had all lapsed", len(s.until))
	}
}

func TestTheBucketRefillsAndIsBoundedByItsBurst(t *testing.T) {
	l := newLimiter()
	now := time.Unix(1_760_000_000, 0)
	taken := 0
	for i := 0; i < 100; i++ {
		if l.take("k", 10, 50, now) {
			taken++
		}
	}
	if taken != 50 {
		t.Fatalf("%d taken from a fresh bucket of 50", taken)
	}
	// One second refills ten, however long it then sits it never exceeds the burst.
	now = now.Add(time.Second)
	taken = 0
	for i := 0; i < 100; i++ {
		if l.take("k", 10, 50, now) {
			taken++
		}
	}
	if taken != 10 {
		t.Errorf("%d taken after one second at 10 per second", taken)
	}
	now = now.Add(time.Hour)
	taken = 0
	for i := 0; i < 100; i++ {
		if l.take("k", 10, 50, now) {
			taken++
		}
	}
	if taken != 50 {
		t.Errorf("%d taken after an idle hour; the burst is 50", taken)
	}
	// Keys do not share.
	if !l.take("other", 10, 50, now) {
		t.Error("an untouched key was refused because another key was empty")
	}
	l.prune(now.Add(limiterIdle + time.Second))
	if len(l.m) != 0 {
		t.Errorf("%d buckets kept after they had all gone idle", len(l.m))
	}
}
