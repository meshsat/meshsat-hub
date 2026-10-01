package takhosted

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// MESHSAT-1461: what a tenant's TAK servers send reaches that tenant's clients,
// what one client exports reaches the tenant's other clients, and neither goes
// anywhere else.
//
// ⚠ The victim in every isolation test is store.DefaultTenantID, per the house
// rule (see cot_ingest_test.go).

// --- fixtures ----------------------------------------------------------------

// online says a bridge of this tenant is reporting in, which is what makes the
// Hub open and hold the tenant's TAK connections.
func online(t *testing.T, b *fakeBus, tenant, bridge string) {
	t.Helper()
	topic := hubmqtt.BridgeTopic(tenant, bridge, "health")
	if n := b.deliver(topic, []byte(`{"uptime":1}`)); n == 0 {
		t.Fatalf("nothing is subscribed to %s, so no tenant is ever seen as online", topic)
	}
}

// fromServer is an event as a TAK server relays it: one line, current.
func fromServer(uid, typ, detail string) string {
	return string(cotEvent(uid, typ, detail)) + "\n"
}

// upstreamCount reads one result of the counter for events from TAK servers.
func upstreamCount(t *testing.T, result string) float64 {
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
			if src == sourceUpstream && res == result {
				return m.GetCounter().GetValue()
			}
		}
		t.Fatalf("no upstream series for result=%q: it is not materialised", result)
	}
	t.Fatal("the counter meshsat_hub_takhosted_cot_total is not registered")
	return 0
}

func waitUpstream(t *testing.T, result string, before, want float64) float64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for upstreamCount(t, result)-before < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return upstreamCount(t, result) - before
}

// waitPublished waits for n messages on a topic.
func waitPublished(t *testing.T, b *fakeBus, topic string, n int) []published {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := b.publishedOn(topic); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return b.publishedOn(topic)
}

// onlyDeliveredOn fails if anything was delivered on a TAK topic other than the
// ones allowed.
func onlyDeliveredOn(t *testing.T, b *fakeBus, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, p := range b.allPublished() {
		if strings.Contains(p.topic, "/tak/cot/in") && !ok[p.topic] {
			t.Errorf("an event was delivered on %s, which is not this tenant's:\n%s", p.topic, p.payload)
		}
	}
}

// --- from a TAK server to the tenant's clients -------------------------------

// The headline: a client that is online and has nothing to send still hears what
// its tenant's TAK server has to say.
func TestAnEventFromTheTAKServerReachesTheTenantsClients(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := upstreamCount(t, cotDelivered)

	// No position, no export: only a kit reporting that it is alive.
	online(t, b, "t1", "kit1")
	ots.connected(t)

	ots.send(t, fromServer("ANDROID-phone-1", "a-f-G-U-C",
		`<contact callsign="ALPHA" endpoint="*:-1:stcp"/><__group name="Cyan" role="Team Member"/>`))

	topic := hubmqtt.TopicTAKBroadcastFor("t1")
	got := waitPublished(t, b, topic, 1)
	if len(got) != 1 {
		t.Fatalf("%d events were delivered on %s, want 1", len(got), topic)
	}
	p := got[0]
	for _, want := range []string{`uid="ANDROID-phone-1"`, `callsign="ALPHA"`, `<__group name="Cyan" role="Team Member"/>`} {
		if !strings.Contains(p.payload, want) {
			t.Errorf("the delivered event lost %s:\n%s", want, p.payload)
		}
	}
	if p.qos != 0 || p.retained {
		t.Errorf("delivered with qos %d retained=%v: a position kept for a client that was offline, "+
			"or handed to the next one to subscribe, is where somebody WAS", p.qos, p.retained)
	}
	if strings.ContainsAny(p.payload, "\r\n") {
		t.Errorf("the delivered event carries a line break: %q", p.payload)
	}
	if moved := waitUpstream(t, cotDelivered, before, 1); moved != 1 {
		t.Errorf("the delivered counter moved by %v, want 1", moved)
	}
	onlyDeliveredOn(t, b, topic)
}

// A server sends nothing to a connection that has not said who it is, and takes
// the first identity it is given. So the Hub's must be first, and must be its own.
func TestTheHubIntroducesItselfBeforeAnythingElse(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)

	// A position is the first thing there is to send.
	deliverPosition(t, b, "t1", "300434", 52.1)
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the position did not arrive: %d events", len(got))
	}

	ots.mu.Lock()
	all := append([]string{}, ots.all...)
	ots.mu.Unlock()
	if len(all) < 2 {
		t.Fatalf("the server received %d lines, want the hello and then the position", len(all))
	}
	hello := all[0]
	for _, want := range []string{`uid="meshsat-hub-t1"`, `callsign="MeshSat Hub"`} {
		if !strings.Contains(hello, want) {
			t.Errorf("the first line on the connection is not the Hub's hello (missing %s):\n%s", want, hello)
		}
	}
	if strings.Contains(hello, `type="a-`) {
		t.Errorf("the hello is a position type, so every TAK client would draw the Hub on its map:\n%s", hello)
	}
	if !strings.Contains(all[1], "meshsat-device-300434") {
		t.Errorf("the second line is not the position:\n%s", all[1])
	}
	// And it parses as an event a server would accept.
	if _, _, err := tak.Sanitize([]byte(hello), tak.UpstreamLimits, time.Now()); err != nil {
		t.Errorf("the hello is not a well-formed event: %v\n%s", err, hello)
	}
}

func TestAnEventFromACustomersTAKServerNeverReachesThePlatformsClients(t *testing.T) {
	platform, customer := newCapturedOTS(t), newCapturedOTS(t)
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-p"}, "t-cust": {"kit-c"}}),
		map[string]*capturedOTS{store.DefaultTenantID: platform, "t-cust": customer})
	online(t, bf.fakeBus, store.DefaultTenantID, "kit-p")
	online(t, bf.fakeBus, "t-cust", "kit-c")
	platform.connected(t)
	customer.connected(t)

	customer.send(t, fromServer("ANDROID-customer", "a-f-G-U-C", `<contact callsign="CUSTOMER"/>`))

	custTopic := hubmqtt.TopicTAKBroadcastFor("t-cust")
	if got := waitPublished(t, bf.fakeBus, custTopic, 1); len(got) != 1 {
		t.Fatalf("the customer's own clients received %d events, want 1", len(got))
	}
	time.Sleep(200 * time.Millisecond)
	platTopic := hubmqtt.TopicTAKBroadcastFor(store.DefaultTenantID)
	if got := bf.publishedOn(platTopic); len(got) != 0 {
		t.Fatalf("a customer's TAK traffic was delivered to the platform's clients on %s:\n%s", platTopic, got[0].payload)
	}
	onlyDeliveredOn(t, bf.fakeBus, custTopic)

	// And the other way, which is also the control: the platform's server is
	// heard, by the platform's clients only.
	platform.send(t, fromServer("ANDROID-platform", "a-f-G-U-C", `<contact callsign="PLATFORM"/>`))
	if got := waitPublished(t, bf.fakeBus, platTopic, 1); len(got) != 1 {
		t.Fatalf("the platform's own clients received %d events, want 1", len(got))
	}
	time.Sleep(200 * time.Millisecond)
	if got := bf.publishedOn(custTopic); len(got) != 1 {
		t.Errorf("the platform's TAK traffic was delivered to a customer's clients: %d events on %s", len(got), custTopic)
	}
	if platTopic != "meshsat/broadcast/tak/cot/in" {
		t.Errorf("the default tenant's topic is %q; the clients in the field subscribe to the historical one", platTopic)
	}
}

// The Hub is not a federation link between a tenant's two servers.
func TestAnEventFromOneUpstreamIsNeverWrittenToAnother(t *testing.T) {
	hosted, own := newCapturedOTS(t), newCapturedOTS(t)
	_, b := forwarderTo(t, storeWithBridges(t, map[string][]string{"t1": {"kit1"}}), hosted.addr, own.addr)
	online(t, b, "t1", "kit1")
	hosted.connected(t)
	own.connected(t)

	hosted.send(t, fromServer("ANDROID-on-hosted", "a-f-G-U-C", `<contact callsign="HOSTED"/>`))

	topic := hubmqtt.TopicTAKBroadcastFor("t1")
	if got := waitPublished(t, b, topic, 1); len(got) != 1 {
		t.Fatalf("the tenant's clients received %d events, want 1", len(got))
	}
	quiet(t, own, "the tenant's other TAK server", 400*time.Millisecond)
	quiet(t, hosted, "the server the event came from", 100*time.Millisecond)
}

// --- from one client to the tenant's other clients ---------------------------

// A server does not send an event back down the connection it arrived on, and
// the Hub is one connection for all of a tenant's clients. So the Hub is what
// shows two kits to each other, and it does so with no TAK server at all.
func TestAClientsCoTReachesItsTenantsOtherClientsWithNoTAKServer(t *testing.T) {
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-solo": {"kit-a", "kit-b"}}),
		map[string]*capturedOTS{}) // nobody has a TAK server
	before := cotCount(t, cotNowhere)

	export(t, bf.fakeBus, "t-solo", "kit-a", cotPLI("MESHSAT-0000aaaa"))

	topic := hubmqtt.TopicTAKBroadcastFromFor("t-solo", "kit-a")
	got := waitPublished(t, bf.fakeBus, topic, 1)
	if len(got) != 1 {
		t.Fatalf("%d events were delivered on %s, want 1", len(got), topic)
	}
	if !strings.Contains(got[0].payload, `uid="MESHSAT-0000aaaa"`) || got[0].qos != 0 || got[0].retained {
		t.Errorf("delivered wrong: qos %d retained=%v\n%s", got[0].qos, got[0].retained, got[0].payload)
	}
	if strings.ContainsAny(got[0].payload, "\r\n") {
		t.Errorf("the delivered event carries a line break: %q", got[0].payload)
	}
	// The sender is the last segment, which is how the client that sent it knows
	// to drop it.
	if !strings.HasSuffix(topic, "/tak/cot/in/kit-a") {
		t.Errorf("the topic %q does not end with the sender", topic)
	}
	if moved := waitCount(t, cotNowhere, before, 1); moved != 1 {
		t.Errorf("accepted with no TAK server should count once as nowhere, moved %v", moved)
	}
	// Nothing on the platform's topics, and nothing on the server topic.
	onlyDeliveredOn(t, bf.fakeBus, topic)
}

func TestOneTenantsExportIsNeverDeliveredToAnotherTenantsClients(t *testing.T) {
	bf := forwarderForTenantsF(t,
		storeWithBridges(t, map[string][]string{store.DefaultTenantID: {"kit-v"}, "t-other": {"kit-o"}}),
		map[string]*capturedOTS{})

	export(t, bf.fakeBus, "t-other", "kit-o", cotPLI("MESHSAT-0000beef"))
	otherTopic := hubmqtt.TopicTAKBroadcastFromFor("t-other", "kit-o")
	if got := waitPublished(t, bf.fakeBus, otherTopic, 1); len(got) != 1 {
		t.Fatalf("the exporting tenant's clients received %d events, want 1", len(got))
	}
	onlyDeliveredOn(t, bf.fakeBus, otherTopic)

	// The control: the platform's own export reaches the platform's own clients.
	export(t, bf.fakeBus, store.DefaultTenantID, "kit-v", cotPLI("MESHSAT-0000cafe"))
	platTopic := hubmqtt.TopicTAKBroadcastFromFor(store.DefaultTenantID, "kit-v")
	if got := waitPublished(t, bf.fakeBus, platTopic, 1); len(got) != 1 {
		t.Fatalf("the platform's clients received %d of the platform's own exports, want 1", len(got))
	}
	if platTopic != "meshsat/broadcast/tak/cot/in/kit-v" {
		t.Errorf("the default tenant's topic is %q", platTopic)
	}
}

// --- what is dropped on the way in -------------------------------------------

// A server is not supposed to send back what a connection wrote. The Hub does not
// build on "not supposed to".
func TestAnEchoOfWhatTheHubSentIsNotFannedOut(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	beforeEcho, beforeHub := upstreamCount(t, cotEcho), upstreamCount(t, cotHubUID)

	sent := cotPLI("MESHSAT-0000aaaa")
	export(t, b, "t1", "kit1", sent)
	deliverPosition(t, b, "t1", "300434", 52.1)
	if got := ots.waitFor(t, 2); len(got) != 2 {
		t.Fatalf("%d events reached the server, want the export and the Hub's own marker", len(got))
	}
	exported, marker := "", ""
	for _, line := range ots.got() {
		if strings.Contains(line, "MESHSAT-0000aaaa") {
			exported = line
		}
		if strings.Contains(line, "meshsat-device-300434") {
			marker = line
		}
	}

	// The server sends both back: the export re-serialised its own way (so the
	// bytes differ), the marker as it was.
	echoed := strings.Replace(exported, `<event `, `<event  access="Undefined" `, 1)
	ots.send(t, echoed+"\n"+marker+"\n")

	if moved := waitUpstream(t, cotEcho, beforeEcho, 1); moved != 1 {
		t.Errorf("%v echoes were recognised, want 1 (the client's export)", moved)
	}
	if moved := waitUpstream(t, cotHubUID, beforeHub, 1); moved != 1 {
		t.Errorf("%v events were dropped for carrying the Hub's own UID, want 1 (its marker)", moved)
	}
	time.Sleep(200 * time.Millisecond)
	if got := b.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")); len(got) != 0 {
		t.Errorf("the Hub delivered its own traffic back to the tenant's clients as news:\n%s", got[0].payload)
	}

	// The control: something the Hub did NOT send is delivered.
	ots.send(t, fromServer("ANDROID-phone-1", "a-f-G-U-C", `<contact callsign="ALPHA"/>`))
	if got := waitPublished(t, b, hubmqtt.TopicTAKBroadcastFor("t1"), 1); len(got) != 1 {
		t.Errorf("after the echoes, a real event was not delivered: %d", len(got))
	}
}

// Keepalives and protocol offers belong to the connection, not to the map.
func TestKeepalivesAndProtocolOffersFromAServerAreNotDelivered(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	before := upstreamCount(t, cotControl)

	ots.send(t, fromServer("srv-pong", "t-x-c-t-r", "")+
		fromServer("protouid", "t-x-takp-v", `<TakControl><TakProtocolSupport version="1"/></TakControl>`)+
		fromServer("meshsat-hub-ping", "t-x-c-t", ""))

	if moved := waitUpstream(t, cotControl, before, 3); moved != 3 {
		t.Errorf("%v control events were counted, want 3", moved)
	}
	time.Sleep(200 * time.Millisecond)
	if got := b.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")); len(got) != 0 {
		t.Errorf("a keepalive or protocol offer was delivered to clients:\n%s", got[0].payload)
	}
}

// The same event from two servers that share traffic is delivered once.
func TestTheSameEventFromTwoServersIsDeliveredOnce(t *testing.T) {
	hosted, own := newCapturedOTS(t), newCapturedOTS(t)
	_, b := forwarderTo(t, storeWithBridges(t, map[string][]string{"t1": {"kit1"}}), hosted.addr, own.addr)
	online(t, b, "t1", "kit1")
	hosted.connected(t)
	own.connected(t)
	before := upstreamCount(t, cotDuplicate)

	ev := fromServer("ANDROID-phone-1", "a-f-G-U-C", `<contact callsign="ALPHA"/>`)
	hosted.send(t, ev)
	own.send(t, ev)

	if moved := waitUpstream(t, cotDuplicate, before, 1); moved != 1 {
		t.Errorf("%v duplicates were counted, want 1", moved)
	}
	if got := b.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")); len(got) != 1 {
		t.Errorf("%d copies were delivered, want 1", len(got))
	}
}

// What a server relays came from whoever is connected to it. It goes through the
// same door as everything else.
func TestAMalformedEventFromAServerIsNotDelivered(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	before := upstreamCount(t, tak.ReasonMarkup) + upstreamCount(t, tak.ReasonStructure) + upstreamCount(t, tak.ReasonFields)

	good := fromServer("ANDROID-ok", "a-f-G-U-C", `<contact callsign="OK"/>`)
	ots.send(t,
		// CDATA hiding an event end: the frame is cut there and refused.
		`<event uid="x" type="a-f-G-U-C"><point lat="1" lon="1"/><detail><remarks><![CDATA[</event>]]></remarks></detail></event>`+"\n"+
			// No point.
			`<event version="2.0" uid="nopoint" type="a-f-G-U-C" time="2026-10-01T20:00:00Z" start="2026-10-01T20:00:00Z" stale="2099-01-01T00:00:00Z"><detail/></event>`+"\n"+
			good)

	topic := hubmqtt.TopicTAKBroadcastFor("t1")
	waitPublished(t, b, topic, 1)
	time.Sleep(200 * time.Millisecond)
	got := b.publishedOn(topic)
	if len(got) != 1 || !strings.Contains(got[0].payload, "ANDROID-ok") {
		t.Fatalf("%d events were delivered, want only the well-formed one:\n%v", len(got), got)
	}
	after := upstreamCount(t, tak.ReasonMarkup) + upstreamCount(t, tak.ReasonStructure) + upstreamCount(t, tak.ReasonFields)
	if after-before < 2 {
		t.Errorf("%v refusals were counted for two malformed events", after-before)
	}
}

// A server cannot make the Hub publish without bound.
func TestAnUpstreamThatFloodsIsThrottled(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	before := upstreamCount(t, cotRateLimited)

	const n = 3000
	var flood strings.Builder
	for i := 0; i < n; i++ {
		flood.WriteString(fromServer(fmt.Sprintf("FLOOD-%05d", i), "a-f-G-U-C", `<contact callsign="F"/>`))
	}
	began := time.Now()
	ots.send(t, flood.String())

	deadline := time.Now().Add(5 * time.Second)
	topic := hubmqtt.TopicTAKBroadcastFor("t1")
	for time.Now().Before(deadline) {
		limited := upstreamCount(t, cotRateLimited) - before
		if int(limited)+len(b.publishedOn(topic)) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	delivered := len(b.publishedOn(topic))
	allowed := upstreamBurst + upstreamRate*time.Since(began).Seconds() + 10
	if float64(delivered) > allowed {
		t.Errorf("%d of %d flood events were delivered; the connection's budget allows about %.0f", delivered, n, allowed)
	}
	if upstreamCount(t, cotRateLimited)-before == 0 {
		t.Error("nothing was rate limited, so the assertion above passed on a reader with no budget")
	}
	if delivered == 0 {
		t.Error("nothing at all was delivered: the budget is not a budget, it is a wall")
	}
}

// --- a stream that cannot be read closes the connection ----------------------

func streamFailures(t *testing.T) float64 {
	t.Helper()
	return counterValue(t, kindHosted, reasonStream)
}

func waitClosed(t *testing.T, f *Forwarder, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for openConns(f) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if openConns(f) != 0 {
		t.Fatalf("the connection is still open after %s", what)
	}
}

func TestAnEventWithNoEndClosesTheConnection(t *testing.T) {
	ots, b, f := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	before := streamFailures(t)

	ots.send(t, `<event uid="endless" type="a-f-G-U-C">`+strings.Repeat("x", tak.UpstreamLimits.MaxBytes+1024))

	waitClosed(t, f, "an event that never ended")
	if moved := streamFailures(t) - before; moved != 1 {
		t.Errorf("the stream failure counter moved by %v, want 1", moved)
	}
	// The link is not dead: the next thing to send dials again.
	deliverPosition(t, b, "t1", "300434", 52.1)
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Errorf("after the connection was closed the next position did not arrive: %d", len(got))
	}
}

func TestAProtobufStreamClosesTheConnection(t *testing.T) {
	ots, b, f := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	before := streamFailures(t)

	ots.send(t, "\xbf\x10\xbf\x0a\x02\x08\x01")

	waitClosed(t, f, "a TAK protocol v1 frame")
	if moved := streamFailures(t) - before; moved != 1 {
		t.Errorf("the stream failure counter moved by %v, want 1", moved)
	}
	if got := b.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")); len(got) != 0 {
		t.Errorf("something was delivered from a protobuf stream: %q", got[0].payload)
	}
}

// The dial probe reads whatever the server says first. Since the stream is read
// now, those bytes are its beginning and must not be dropped.
func TestTheProbeDoesNotEatTheFirstBytesOfTheStream(t *testing.T) {
	ots := newCapturedOTS(t)
	// A server that speaks the moment a connection is accepted.
	ots.greet = fromServer("ANDROID-early", "a-f-G-U-C", `<contact callsign="EARLY"/>`)
	bf := forwarderForTenantsF(t, storeWithBridges(t, map[string][]string{"t1": {"kit1"}}),
		map[string]*capturedOTS{"t1": ots})

	online(t, bf.fakeBus, "t1", "kit1")

	got := waitPublished(t, bf.fakeBus, hubmqtt.TopicTAKBroadcastFor("t1"), 1)
	if len(got) != 1 || !strings.Contains(got[0].payload, `uid="ANDROID-early"`) {
		t.Fatalf("an event the server sent as the connection opened was lost; the probe dropped "+
			"its first bytes: %v", got)
	}
}

// --- connections follow the clients ------------------------------------------

func TestATenantWithAClientOnlineKeepsItsConnection(t *testing.T) {
	ots, b, f := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)
	r := f.cur.Load()

	// Long after the last event, with the client still reporting in.
	later := time.Now().Add(idleUpstream + time.Hour)
	r.present.touch("t1", later)
	r.reapIdle(f.log, later)
	if openConns(f) != 1 {
		t.Fatal("a tenant with a client online lost its connection for having nothing to send: " +
			"a kit with no fix would never see its team")
	}

	// The client gone, the connection is no longer held open for it.
	gone := later.Add(presenceTTL + time.Minute)
	r.reapIdle(f.log, gone)
	deadline := time.Now().Add(2 * time.Second)
	for openConns(f) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if openConns(f) != 0 {
		t.Error("a tenant with nobody online and nothing sent for over fifteen minutes kept its connection")
	}
}

func TestPresenceLapsesAndArrivalsAreReportedOnce(t *testing.T) {
	p := newPresence()
	now := time.Unix(1_760_000_000, 0)
	if !p.touch("t1", now) {
		t.Error("the first report from a tenant was not an arrival")
	}
	if p.touch("t1", now.Add(30*time.Second)) {
		t.Error("a tenant already present arrived again: its connections would be asked for on every health report")
	}
	if !p.present("t1", now.Add(presenceTTL)) || p.present("t1", now.Add(30*time.Second+presenceTTL+time.Second)) {
		t.Error("presence does not lapse presenceTTL after the last report")
	}
	if got := p.active(now.Add(time.Hour)); len(got) != 0 || len(p.last) != 0 {
		t.Errorf("a lapsed tenant is still listed or still held: %v", got)
	}
	if !p.touch("t1", now.Add(2*time.Hour)) {
		t.Error("a tenant coming back after lapsing was not an arrival")
	}
}

// Losing the lease closes every connection and waits for its reader, so nothing
// of this tenure is still delivering when the next leader starts.
func TestLosingTheLeaseClosesEveryReader(t *testing.T) {
	ots := newCapturedOTS(t)
	b := newFakeBus()
	f := NewForwarder(b, storeWithBridges(t, map[string][]string{"t1": {"kit1"}}), plainDial, oneTenant(ots.addr), nil)
	lose := startRun(t, f)
	online(t, b, "t1", "kit1")
	ots.connected(t)

	lose() // fails the test itself if Run does not return, which is a reader still running

	ots.send(t, fromServer("ANDROID-late", "a-f-G-U-C", `<contact callsign="LATE"/>`))
	time.Sleep(300 * time.Millisecond)
	if got := b.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")); len(got) != 0 {
		t.Errorf("a replica that had lost the lease delivered an event from a TAK server:\n%s", got[0].payload)
	}
}

// A connection that has gone quiet is pinged, which is how a peer that left
// without closing is noticed.
func TestASilentConnectionIsPinged(t *testing.T) {
	after, check := linkPingAfter, linkPingCheck
	linkPingAfter, linkPingCheck = 80*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { linkPingAfter, linkPingCheck = after, check })

	ots, b, _ := oneTenantWithBridge(t)
	online(t, b, "t1", "kit1")
	ots.connected(t)

	deadline := time.Now().Add(3 * time.Second)
	pinged := func() bool {
		for _, line := range ots.hellos() {
			if strings.Contains(line, `uid="meshsat-hub-ping"`) && strings.Contains(line, `type="t-x-c-t"`) {
				return true
			}
		}
		return false
	}
	for !pinged() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !pinged() {
		t.Errorf("a silent connection was never pinged; the server received:\n%v", ots.hellos())
	}
}

// The Hub's own UIDs are refused from a client as well as from a server.
func TestAClientCannotSpeakAsTheHubOnATAKStream(t *testing.T) {
	ots, b, _ := oneTenantWithBridge(t)
	before := cotCount(t, cotHubUID)
	export(t, b, "t1", "kit1", cotPLI("meshsat-hub-t1"))
	export(t, b, "t1", "kit1", cotPLI("meshsat-hub-ping"))
	if moved := waitCount(t, cotHubUID, before, 2); moved != 2 {
		t.Fatalf("%v events were refused for carrying the Hub's stream identity, want 2", moved)
	}
	quiet(t, ots, "the TAK server", 300*time.Millisecond)
}
