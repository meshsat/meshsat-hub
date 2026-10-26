package tak

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// What Sanitize records of a GeoChat (MESHSAT-1706), as each client writes it.

const (
	atakDirect = `<event version="2.0" uid="GeoChat.ANDROID-abc.meshsat-a1b3c3a4.m1" type="b-t-f" how="h-g-i-g-o" time="%[1]s" start="%[1]s" stale="%[2]s">` +
		`<point lat="52.1" lon="4.5" hae="9999999.0" ce="9999999.0" le="9999999.0"/><detail>` +
		`<__chat parent="RootContactGroup" groupOwner="false" messageId="m1" chatroom="MESHSAT-c3a4" id="meshsat-a1b3c3a4" senderCallsign="DISPATCH-1">` +
		`<chatgrp uid0="ANDROID-abc" uid1="meshsat-a1b3c3a4" id="meshsat-a1b3c3a4"/></__chat>` +
		`<link uid="ANDROID-abc" type="a-f-G-U-C" relation="p-p"/>` +
		`<remarks source="BAO.F.ATAK.ANDROID-abc" to="meshsat-a1b3c3a4" time="%[1]s">check &amp; report</remarks>` +
		`<marti><dest callsign="MESHSAT-c3a4"/></marti></detail></event>`
	itakDirect = `<event version="2.0" uid="GeoChat.ITAK-1.meshsat-a1b3c3a4.m2" type="b-t-f" how="h-g-i-g-o" time="%[1]s" start="%[1]s" stale="%[2]s">` +
		`<point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail>` +
		`<__chat parent="RootContactGroup" groupOwner="false" messageId="m2" chatroom="MESHSAT-c3a4" id="meshsat-a1b3c3a4" senderCallsign="ITAK-OPS">` +
		`<chatgrp uid0="ITAK-1" uid1="meshsat-a1b3c3a4" id="meshsat-a1b3c3a4"/></__chat>` +
		`<link uid="ITAK-1" type="a-f-G-U-C" relation="p-p"/><remarks>go north</remarks>` +
		`<marti><dest uid="meshsat-a1b3c3a4"/><dest mission="ops"/></marti></detail></event>`
	roomChat = `<event version="2.0" uid="GeoChat.ANDROID-abc.All Chat Rooms.m3" type="b-t-f" how="h-g-i-g-o" time="%[1]s" start="%[1]s" stale="%[2]s">` +
		`<point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail>` +
		`<__chat chatroom="All Chat Rooms" id="All Chat Rooms" senderCallsign="DISPATCH-1"/>` +
		`<link uid="ANDROID-abc" type="a-f-G-U-C" relation="p-p"/><remarks>everyone hold</remarks></detail></event>`
)

func sanitizeAt(t *testing.T, doc string, now time.Time) Meta {
	t.Helper()
	raw := fmt.Sprintf(doc, now.UTC().Format(time.RFC3339), now.Add(time.Hour).UTC().Format(time.RFC3339))
	_, meta, err := Sanitize([]byte(raw), ClientLimits, now)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	return meta
}

func TestSanitizeRecordsAChatsRouting(t *testing.T) {
	now := time.Now()
	crew := Dest{UID: "meshsat-a1b3c3a4", Callsign: "MESHSAT-c3a4"}
	cases := []struct {
		name               string
		doc                string
		to                 []Dest
		fromUID, fromCS    string
		remarks, messageID string
	}{
		{"atak names the callsign", atakDirect, []Dest{crew}, "ANDROID-abc", "DISPATCH-1", "check & report", "m1"},
		{"itak names the uid; a mission dest is no one", itakDirect, []Dest{crew}, "ITAK-1", "ITAK-OPS", "go north", "m2"},
		{"the room names nobody", roomChat, []Dest{}, "ANDROID-abc", "DISPATCH-1", "everyone hold", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := sanitizeAt(t, c.doc, now)
			if !m.IsChat() {
				t.Fatal("not a chat")
			}
			if got := m.Addressees(); fmt.Sprint(got) != fmt.Sprint(c.to) {
				t.Errorf("addressees %v, want %v", got, c.to)
			}
			if uid, cs := m.Sender(); uid != c.fromUID || cs != c.fromCS {
				t.Errorf("sender %q %q", uid, cs)
			}
			if m.Remarks != c.remarks || m.ChatMessageID != c.messageID {
				t.Errorf("remarks %q message id %q", m.Remarks, m.ChatMessageID)
			}
		})
	}
}

func TestSanitizeRecordsAPositionsCallsign(t *testing.T) {
	now := time.Now()
	raw := fmt.Sprintf(`<event version="2.0" uid="meshsat-a1b3c3a4" type="a-f-G-U-C" how="m-g" time="%[1]s" start="%[1]s" stale="%[2]s">`+
		`<point lat="52" lon="4" hae="0" ce="10" le="10"/><detail><contact callsign="MESHSAT-c3a4" endpoint="*:-1:stcp"/></detail></event>`,
		now.UTC().Format(time.RFC3339), now.Add(time.Minute).UTC().Format(time.RFC3339))
	_, m, err := Sanitize([]byte(raw), ClientLimits, now)
	if err != nil {
		t.Fatal(err)
	}
	if m.Callsign != "MESHSAT-c3a4" || m.IsChat() || len(m.Addressees()) != 0 {
		t.Errorf("callsign %q chat %v addressees %v", m.Callsign, m.IsChat(), m.Addressees())
	}
}

func TestSanitizeCapsRemarks(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("x", MaxRemarks+500)
	doc := strings.Replace(roomChat, "everyone hold", long, 1)
	raw := fmt.Sprintf(doc, now.UTC().Format(time.RFC3339), now.Add(time.Hour).UTC().Format(time.RFC3339))
	_, m, err := Sanitize([]byte(raw), UpstreamLimits, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Remarks) != MaxRemarks {
		t.Errorf("remarks %d bytes, want the cap %d", len(m.Remarks), MaxRemarks)
	}
}

func TestBuildGeoChatPassesSanitizeAndReadsBack(t *testing.T) {
	now := time.Now()
	from := ChatPeer{UID: "meshsat-hub-op-7", Callsign: "Office"}
	to := ChatPeer{UID: "meshsat-a1b3c3a4", Callsign: "MESHSAT-c3a4"}
	xml, err := MarshalCotEvent(BuildGeoChat(from, to, `come in <now> & "fast"`, "id-1", now))
	if err != nil {
		t.Fatal(err)
	}
	_, m, err := Sanitize(xml, ClientLimits, now)
	if err != nil {
		t.Fatalf("the Hub's own GeoChat is refused by its own sanitiser: %v", err)
	}
	if got := m.Addressees(); len(got) != 1 || got[0] != Dest(to) {
		t.Errorf("addressees %v", got)
	}
	if uid, cs := m.Sender(); uid != from.UID || cs != from.Callsign {
		t.Errorf("sender %q %q", uid, cs)
	}
	if m.Remarks != `come in <now> & "fast"` || m.ChatMessageID != "id-1" {
		t.Errorf("remarks %q id %q", m.Remarks, m.ChatMessageID)
	}
}
