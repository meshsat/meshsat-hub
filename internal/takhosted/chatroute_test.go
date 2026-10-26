package takhosted

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Addressed events and the crews' owners (MESHSAT-1706). Every "not delivered"
// has a positive control beside it, and the victim of every isolation test is
// store.DefaultTenantID.

// --- fixtures ----------------------------------------------------------------

func crewPLI(uid, callsign string) []byte {
	return cotEvent(uid, "a-f-G-U-C", `<contact callsign="`+callsign+`" endpoint="*:-1:stcp"/>`)
}

// geoChat is a GeoChat in ATAK's shape; dest is "callsign", "uid" or "" (the
// room, no marti).
func geoChat(fromUID, fromCS, toUID, toCS, dest, text string) []byte {
	marti := ""
	switch dest {
	case "callsign":
		marti = `<marti><dest callsign="` + toCS + `"/></marti>`
	case "uid":
		marti = `<marti><dest uid="` + toUID + `"/></marti>`
	}
	return cotEvent("GeoChat."+fromUID+"."+toUID+"."+text, "b-t-f",
		`<__chat parent="RootContactGroup" groupOwner="false" messageId="`+text+`" chatroom="`+toCS+`" id="`+toUID+`" senderCallsign="`+fromCS+`">`+
			`<chatgrp uid0="`+fromUID+`" uid1="`+toUID+`" id="`+toUID+`"/></__chat>`+
			`<link uid="`+fromUID+`" type="a-f-G-U-C" relation="p-p"/>`+
			`<remarks source="BAO.F.ATAK.`+fromUID+`" to="`+toUID+`">`+text+`</remarks>`+marti)
}

// chatRig is tenant t1 with kits kit-a and kit-b and phone phone-1, one hosted
// TAK server, dispatcher-to-crew routing on; the default tenant has kit-v.
func chatRig(t *testing.T) (busAndForwarder, *capturedOTS, store.Store) {
	t.Helper()
	s := storeWithBridges(t, map[string][]string{
		store.DefaultTenantID: {"kit-v"},
		"t1":                  {"kit-a", "kit-b", "phone-1"},
	})
	ots := newCapturedOTS(t)
	bf := forwarderForTenantsF(t, s, map[string]*capturedOTS{"t1": ots})
	bf.f.SetChat(true)
	online(t, bf.fakeBus, "t1", "kit-a")
	ots.connected(t)
	return bf, ots, s
}

// owns waits until the leader takes bridge as the crew's owner.
func owns(t *testing.T, f *Forwarder, tenant, uid, bridge string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.cur.Load(); r != nil {
			if got, _, ok := r.owners.owner(tenant, tak.Dest{UID: uid}, time.Now()); ok && got == bridge {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became the owner of %s in %s", bridge, uid, tenant)
}

func relayLines(ots *capturedOTS) []string {
	var out []string
	for _, l := range ots.hellos() {
		if strings.Contains(l, `type="`+relayType+`"`) {
			out = append(out, l)
		}
	}
	return out
}

func waitRelayNaming(t *testing.T, ots *capturedOTS, uid string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range relayLines(ots) {
			if strings.Contains(l, `uid="`+uid+`"`) {
				return l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no relay frame naming %s reached the TAK server; frames: %v", uid, relayLines(ots))
	return ""
}

// --- ownership and the relay frame -------------------------------------------

func TestAnExportedPositionMakesItsBridgeTheCrewsOwnerAndTellsTheServer(t *testing.T) {
	bf, ots, s := chatRig(t)
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0a0b0c0d", "MESHSAT-0c0d"))
	owns(t, bf.f, "t1", "meshsat-0a0b0c0d", "kit-a")
	frame := waitRelayNaming(t, ots, "meshsat-0a0b0c0d")
	if !strings.Contains(frame, `callsign="MESHSAT-0c0d"`) || !strings.Contains(frame, `uid="`+hubUIDPrefix+`relay"`) {
		t.Errorf("relay frame: %s", frame)
	}
	// Written behind to the store, for the API on either replica.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := s.GetTAKContact(context.Background(), "t1", "meshsat-0a0b0c0d")
		if err == nil && c != nil && c.BridgeID == "kit-a" && c.Callsign == "MESHSAT-0c0d" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the contact was not stored: %+v %v", c, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The victim tenant knows nothing of it.
	if c, _ := s.GetTAKContact(context.Background(), store.DefaultTenantID, "meshsat-0a0b0c0d"); c != nil {
		t.Errorf("the contact leaked into the default tenant: %+v", c)
	}
}

func TestNoRelayFrameWhileChatIsOff(t *testing.T) {
	bf, ots, _ := chatRig(t)
	bf.f.SetChat(false)
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0000aaaa", "MESHSAT-aaaa"))
	// Positive control: the position itself reached the server.
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the position did not reach the server: %v", got)
	}
	if lines := relayLines(ots); len(lines) != 0 {
		t.Errorf("a relay frame went out with chat off: %v", lines)
	}
}

func TestNoRelayFrameToATenantsOwnServer(t *testing.T) {
	s := storeWithBridges(t, map[string][]string{"t1": {"kit-a"}})
	ots := newCapturedOTS(t)
	// The tenant's own server: a stock one would hand the frame to every client.
	own := &takfront.Tenant{TenantID: "t1", Label: externalLabel, Upstream: ots.addr}
	b := newFakeBus()
	f := NewForwarder(b, s, plainDial, func(_ context.Context, id string) []*takfront.Tenant {
		if id == "t1" {
			return []*takfront.Tenant{own}
		}
		return nil
	}, nil)
	f.SetChat(true)
	startRun(t, f)
	online(t, b, "t1", "kit-a")
	ots.connected(t)
	export(t, b, "t1", "kit-a", crewPLI("meshsat-0000bbbb", "MESHSAT-bbbb"))
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the position did not reach the tenant's server: %v", got)
	}
	time.Sleep(200 * time.Millisecond)
	if lines := relayLines(ots); len(lines) != 0 {
		t.Errorf("a relay frame went to a tenant's own server: %v", lines)
	}
}

// --- from a TAK server: a dispatcher's chat to one crew ----------------------

func TestADispatchersChatReachesOnlyTheClientThatOwnsTheCrew(t *testing.T) {
	bf, ots, _ := chatRig(t)
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0a0b0c0d", "MESHSAT-0c0d"))
	export(t, bf.fakeBus, "t1", "kit-b", crewPLI("meshsat-11112222", "MESHSAT-2222"))
	owns(t, bf.f, "t1", "meshsat-0a0b0c0d", "kit-a")
	owns(t, bf.f, "t1", "meshsat-11112222", "kit-b")
	before := upstreamCount(t, cotRouted)

	for _, dest := range []string{"callsign", "uid"} {
		ots.send(t, string(geoChat("ANDROID-abc", "DISPATCH-1", "meshsat-0a0b0c0d", "MESHSAT-0c0d", dest, "chk-"+dest))+"\n")
	}
	toA := hubmqtt.TopicTAKCotInFor("t1", "kit-a")
	got := waitPublished(t, bf.fakeBus, toA, 2)
	if len(got) != 2 {
		t.Fatalf("kit-a got %d chats, want 2 (by callsign and by uid)", len(got))
	}
	for _, p := range got {
		if p.qos != 1 || p.retained || !strings.Contains(p.payload, `type="b-t-f"`) {
			t.Errorf("delivered wrong: qos %d retained %v\n%s", p.qos, p.retained, p.payload)
		}
	}
	if moved := waitUpstream(t, cotRouted, before, 2); moved != 2 {
		t.Errorf("routed moved %v, want 2", moved)
	}
	// Nobody else: not kit-b, not the broadcast, not the victim tenant.
	onlyDeliveredOn(t, bf.fakeBus, toA,
		hubmqtt.TopicTAKBroadcastFromFor("t1", "kit-a"), hubmqtt.TopicTAKBroadcastFromFor("t1", "kit-b"))
}

func TestAChatForNoKnownCrewIsDroppedNeverBroadcast(t *testing.T) {
	bf, ots, _ := chatRig(t)
	before := upstreamCount(t, cotNoOwner)
	ots.send(t, string(geoChat("ANDROID-abc", "DISPATCH-1", "meshsat-99999999", "MESHSAT-9999", "callsign", "lost"))+"\n")
	if moved := waitUpstream(t, cotNoOwner, before, 1); moved != 1 {
		t.Fatalf("noowner moved %v, want 1", moved)
	}
	// Positive control: an event naming nobody still reaches the tenant's clients.
	ots.send(t, fromServer("ANDROID-xyz", "a-f-G-U-C", `<contact callsign="OTHER"/>`))
	if got := waitPublished(t, bf.fakeBus, hubmqtt.TopicTAKBroadcastFor("t1"), 1); len(got) != 1 {
		t.Fatalf("the control event was not delivered")
	}
	for _, p := range bf.publishedOn(hubmqtt.TopicTAKBroadcastFor("t1")) {
		if strings.Contains(p.payload, "lost") {
			t.Errorf("an addressed chat was broadcast: %s", p.payload)
		}
	}
}

func TestATAKUserCannotBeTakenAsACrew(t *testing.T) {
	bf, ots, _ := chatRig(t)
	// The server shows its own user DISPATCH-1.
	ots.send(t, fromServer("ANDROID-abc", "a-f-G-U-C", `<contact callsign="DISPATCH-1"/>`))
	waitPublished(t, bf.fakeBus, hubmqtt.TopicTAKBroadcastFor("t1"), 1)
	// A client exports a position under the same callsign: no owner.
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0000dddd", "DISPATCH-1"))
	// Positive control: an honest crew from the same client is owned.
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0000eeee", "MESHSAT-eeee"))
	owns(t, bf.f, "t1", "meshsat-0000eeee", "kit-a")
	r := bf.f.cur.Load()
	if _, _, ok := r.owners.owner("t1", tak.Dest{UID: "meshsat-0000dddd"}, time.Now()); ok {
		t.Error("a client took a TAK user's callsign as its crew")
	}
	if _, _, ok := r.owners.owner("t1", tak.Dest{Callsign: "DISPATCH-1"}, time.Now()); ok {
		t.Error("a chat to a TAK user's callsign would go to a client")
	}
}

func TestAPhonesOwnUIDIsOwnedOnlyByThatPhone(t *testing.T) {
	bf, _, _ := chatRig(t)
	// (Different bytes: the same event from two senders inside a minute is one.)
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("MESHSAT-phone-1", "TAKEN-BY-KIT"))
	export(t, bf.fakeBus, "t1", "phone-1", crewPLI("MESHSAT-phone-1", "MESHSAT-NE-1"))
	owns(t, bf.f, "t1", "MESHSAT-phone-1", "phone-1")
	if r := bf.f.cur.Load(); r != nil {
		if _, _, ok := r.owners.owner("t1", tak.Dest{Callsign: "TAKEN-BY-KIT"}, time.Now()); ok {
			t.Error("a kit took a phone's own uid")
		}
	}
	// And a kit's own MESHSAT-<node id>, which names no bridge, is the kit's.
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("MESHSAT-0d0e0f10", "MESHSAT-0F10"))
	owns(t, bf.f, "t1", "MESHSAT-0d0e0f10", "kit-a")
}

// --- from a client: what an addressed event may not do ------------------------

func TestAClientsAddressedEventIsNeverFannedOutToOtherClients(t *testing.T) {
	bf, ots, _ := chatRig(t)
	bf.f.SetChat(false) // the leak fix holds with chat off too
	export(t, bf.fakeBus, "t1", "kit-a", geoChat("meshsat-0a0b0c0d", "MESHSAT-0c0d", "ANDROID-abc", "DISPATCH-1", "callsign", "reply-1"))
	// To the server, which routes it to the dispatcher.
	got := ots.waitFor(t, 1)
	if len(got) != 1 || !strings.Contains(got[0], "reply-1") {
		t.Fatalf("the answer did not reach the TAK server: %v", got)
	}
	// Positive control: an unaddressed event of the same client is fanned out.
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0000ffff", "MESHSAT-ffff"))
	fan := waitPublished(t, bf.fakeBus, hubmqtt.TopicTAKBroadcastFromFor("t1", "kit-a"), 1)
	if len(fan) != 1 {
		t.Fatalf("the control was not fanned out")
	}
	for _, p := range fan {
		if strings.Contains(p.payload, "reply-1") {
			t.Errorf("an addressed answer was fanned out to the other clients: %s", p.payload)
		}
	}
}

func TestACrewsAnswerToAnOperatorStaysOnTheHub(t *testing.T) {
	bf, ots, _ := chatRig(t)
	before := cotCount(t, cotOperator)
	export(t, bf.fakeBus, "t1", "kit-a", geoChat("meshsat-0a0b0c0d", "MESHSAT-0c0d", OperatorUIDPrefix+"7", "Office", "uid", "op-answer"))
	if moved := waitCount(t, cotOperator, before, 1); moved != 1 {
		t.Fatalf("operator moved %v, want 1", moved)
	}
	// Positive control: the next position reaches the server; the answer did not.
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-0000abcd", "MESHSAT-abcd"))
	got := ots.waitFor(t, 1)
	if len(got) != 1 || strings.Contains(strings.Join(got, "\n"), "op-answer") {
		t.Errorf("server got %v", got)
	}
	onlyDeliveredOn(t, bf.fakeBus, hubmqtt.TopicTAKBroadcastFromFor("t1", "kit-a"))
}

func TestAChatInAnOperatorsNameFromAClientIsRefused(t *testing.T) {
	bf, ots, _ := chatRig(t)
	before := cotCount(t, cotHubUID)
	export(t, bf.fakeBus, "t1", "kit-a", geoChat(OperatorUIDPrefix+"7", "Office", "ANDROID-abc", "DISPATCH-1", "callsign", "forged"))
	if moved := waitCount(t, cotHubUID, before, 1); moved != 1 {
		t.Fatalf("hubuid moved %v, want 1", moved)
	}
	export(t, bf.fakeBus, "t1", "kit-a", crewPLI("meshsat-00001234", "MESHSAT-1234"))
	if got := ots.waitFor(t, 1); len(got) != 1 || strings.Contains(got[0], "forged") {
		t.Errorf("server got %v", got)
	}
}

func TestACrewWritingToAnotherClientsCrewReachesThatClient(t *testing.T) {
	bf, ots, _ := chatRig(t)
	export(t, bf.fakeBus, "t1", "kit-b", crewPLI("meshsat-11112222", "MESHSAT-2222"))
	owns(t, bf.f, "t1", "meshsat-11112222", "kit-b")
	export(t, bf.fakeBus, "t1", "kit-a", geoChat("meshsat-0a0b0c0d", "MESHSAT-0c0d", "meshsat-11112222", "MESHSAT-2222", "uid", "crew-to-crew"))
	toB := hubmqtt.TopicTAKCotInFor("t1", "kit-b")
	if got := waitPublished(t, bf.fakeBus, toB, 1); len(got) != 1 || !strings.Contains(got[0].payload, "crew-to-crew") {
		t.Fatalf("kit-b got %v", got)
	}
	// Not to the server: no TAK user is named.
	time.Sleep(100 * time.Millisecond)
	for _, l := range ots.got() {
		if strings.Contains(l, "crew-to-crew") {
			t.Errorf("a crew-to-crew chat went to the TAK server: %s", l)
		}
	}
}

// --- the owners themselves ---------------------------------------------------

func TestOwnershipIsSticky(t *testing.T) {
	o := newOwners()
	now := time.Now()
	if owns, _ := o.observe("t1", "kit-a", "meshsat-1", "C1", now); !owns {
		t.Fatal("the first exporter does not own the crew")
	}
	if owns, _ := o.observe("t1", "kit-b", "meshsat-1", "C1", now.Add(ownerSticky-time.Second)); owns {
		t.Error("another bridge took the crew while the owner was current")
	}
	if b, _, _ := o.owner("t1", tak.Dest{UID: "meshsat-1"}, now.Add(time.Minute)); b != "kit-a" {
		t.Errorf("owner %q", b)
	}
	if owns, _ := o.observe("t1", "kit-b", "meshsat-1", "C1", now.Add(ownerSticky+time.Second)); !owns {
		t.Error("the crew did not move once the owner had been quiet")
	}
	if _, _, ok := o.owner("t1", tak.Dest{UID: "meshsat-1"}, now.Add(ownerSticky+ownerFresh+time.Minute)); ok {
		t.Error("a crew quiet for a day is still reachable")
	}
}

func TestACallsignTwoCrewsShareResolvesToNeither(t *testing.T) {
	o := newOwners()
	now := time.Now()
	o.observe("t1", "kit-a", "meshsat-11110c0d", "MESHSAT-0c0d", now)
	if b, uid, ok := o.owner("t1", tak.Dest{Callsign: "MESHSAT-0c0d"}, now); !ok || b != "kit-a" || uid != "meshsat-11110c0d" {
		t.Fatalf("one crew with the callsign: %q %q %v", b, uid, ok)
	}
	o.observe("t1", "kit-b", "meshsat-22220c0d", "MESHSAT-0c0d", now)
	if _, _, ok := o.owner("t1", tak.Dest{Callsign: "MESHSAT-0c0d"}, now); ok {
		t.Error("an ambiguous callsign resolved")
	}
	if b, _, ok := o.owner("t1", tak.Dest{UID: "meshsat-22220c0d"}, now); !ok || b != "kit-b" {
		t.Error("the uid no longer resolves")
	}
}

func TestTheRelaySetIsCappedNewestFirst(t *testing.T) {
	o := newOwners()
	now := time.Now()
	for i := 0; i < relayMax+10; i++ {
		o.observe("t1", "kit-a", fmt.Sprintf("meshsat-%08x", i), fmt.Sprintf("C%d", i), now.Add(time.Duration(i)*time.Millisecond))
	}
	set := o.relaySet("t1", now.Add(time.Hour))
	if len(set) != relayMax {
		t.Fatalf("%d crews in the frame", len(set))
	}
	if set[0].UID != fmt.Sprintf("meshsat-%08x", relayMax+9) {
		t.Errorf("the newest crew is not first: %v", set[0])
	}
	if !o.relayDue("t1", now) || o.relayDue("t1", now.Add(time.Second)) {
		t.Error("relayDue after a change, then not again until relayEvery")
	}
	if !o.relayDue("t1", now.Add(relayEvery+time.Second)) {
		t.Error("not due after relayEvery")
	}
}

func TestOwnersLoadedBackDoNotReplaceWhatThisTenureSaw(t *testing.T) {
	o := newOwners()
	now := time.Now()
	o.observe("t1", "kit-b", "meshsat-1", "C1", now)
	o.load("t1", []store.TAKContact{
		{UID: "meshsat-1", Callsign: "C1", BridgeID: "kit-a", LastSeen: now.Add(-time.Hour)},
		{UID: "meshsat-2", Callsign: "C2", BridgeID: "kit-a", LastSeen: now.Add(-time.Hour)},
	})
	if b, _, _ := o.owner("t1", tak.Dest{UID: "meshsat-1"}, now); b != "kit-b" {
		t.Errorf("a stored owner replaced a live one: %q", b)
	}
	if b, _, ok := o.owner("t1", tak.Dest{UID: "meshsat-2"}, now); !ok || b != "kit-a" {
		t.Errorf("a stored owner was not loaded: %q %v", b, ok)
	}
	if !o.isLoaded("t1") || o.isLoaded(store.DefaultTenantID) {
		t.Error("loaded flags")
	}
}

func TestTheRelayEventEscapesItsNames(t *testing.T) {
	ev := relayEvent([]tak.ChatPeer{{UID: `a"b`, Callsign: "c<d&"}}, time.Now())
	_, _, err := tak.Sanitize(ev, tak.ClientLimits, time.Now())
	if err != nil {
		t.Fatalf("the relay event is not well-formed CoT: %v\n%s", err, ev)
	}
	if !strings.Contains(string(ev), `uid="a&#34;b"`) || !strings.Contains(string(ev), `callsign="c&lt;d&amp;"`) {
		t.Errorf("not escaped: %s", ev)
	}
}
