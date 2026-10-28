package warnings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// MESHSAT-1734: a warning in force is handed to the kits again every
// repeat_hours, once per (tenant, kit, message, slot) across the replicas.
// The clock is injected; the slots count from the warning's sent time.

// texelSent is the sent time of the MeteoAlarm fixture; it expires 24 h later.
var texelSent = time.Date(2026, 10, 28, 10, 11, 41, 0, time.UTC)

func sendsTo(bus *fakeBus) map[string]int {
	out := map[string]int{}
	for _, s := range bus.published() {
		out[s.topic]++
	}
	return out
}

func TestRepeatWhileInForce(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	feed := &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}}
	svc, bus, db, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804"}}, feed) // repeat_hours unset: 6
	ctx := context.Background()
	clock := texelSent.Add(time.Minute)
	svc.now = func() time.Time { return clock }
	texel, leiden := "meshsat/t1/bridge/texel/warn/in", "meshsat/t1/bridge/leiden/warn/in"

	svc.Poll(ctx)
	svc.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 1 || got[leiden] != 1 || len(got) != 2 {
		t.Fatalf("first delivery: %v", got)
	}
	// Before the first slot: nothing.
	clock = texelSent.Add(3 * time.Hour)
	svc.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 1 || got[leiden] != 1 {
		t.Fatalf("a repeat before the slot: %v", got)
	}
	// Slot 1: every kit again, the phone still not; a second pass at the
	// same time, and another replica, hand nothing more.
	clock = texelSent.Add(6*time.Hour + time.Minute)
	svc.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 2 || got[leiden] != 2 || len(got) != 2 {
		t.Fatalf("slot 1: %v", got)
	}
	svc.Repeat(ctx)
	bus2 := &fakeBus{}
	svc2 := New(db, bus2, svc.accounts, newTestSigner(t), nil, nil, time.Minute, nil)
	svc2.now = svc.now
	svc2.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 2 || got[leiden] != 2 || len(bus2.published()) != 0 {
		t.Fatalf("slot 1 handed twice: %v, other replica %d", got, len(bus2.published()))
	}
	// The repeat is the same digest, verified by the same key.
	last := bus.published()[len(bus.published())-1]
	if d, err := Verify(svc.PublicKey(), last.payload); err != nil || d.ID != IDOf(a.Identifier) || d.MsgType != TypeUpdate {
		t.Fatalf("the repeated digest: %+v %v", d, err)
	}
	// Slot 2, and the record says when each kit last got it.
	clock = texelSent.Add(12*time.Hour + time.Minute)
	svc.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 3 || got[leiden] != 3 {
		t.Fatalf("slot 2: %v", got)
	}
	list, err := db.ListTenantWarnings(ctx, "t1", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	var detail matchDetail
	if err := json.Unmarshal([]byte(list[0].Detail), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repeat != 2 || detail.RepeatAt == nil || !detail.RepeatAt.Equal(clock) ||
		detail.Handed["texel"].Slot != 2 || !detail.Handed["texel"].At.Equal(clock) || detail.Handed["leiden"].Bearers[0] != "mqtt" {
		t.Fatalf("detail: %s", list[0].Detail)
	}
	if !list[0].MatchedAt.Equal(texelSent.Add(time.Minute)) {
		t.Fatalf("a repeat moved the match time: %v", list[0].MatchedAt)
	}
	// Expired: nothing more, however many slots pass.
	clock = texelSent.Add(24*time.Hour + time.Minute)
	svc.Repeat(ctx)
	clock = texelSent.Add(48 * time.Hour)
	svc.Repeat(ctx)
	if got := sendsTo(bus); got[texel] != 3 || got[leiden] != 3 {
		t.Fatalf("an expired warning was repeated: %v", got)
	}
}

func TestRepeatOffAndRepeatHoursBounds(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	feed := &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}}
	svc, bus, _, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804", "repeat_hours": "0"}}, feed)
	ctx := context.Background()
	clock := texelSent.Add(time.Minute)
	svc.now = func() time.Time { return clock }
	svc.Poll(ctx)
	for _, h := range []time.Duration{6, 12, 23} {
		clock = texelSent.Add(h*time.Hour + time.Minute)
		svc.Repeat(ctx)
	}
	if n := len(bus.published()); n != 2 {
		t.Fatalf("repeat_hours 0 repeated: %d sends", n)
	}
	// A short period repeats more often.
	svc.accounts = fakeAccounts{"t1": {"enabled": "true", "areas": "NL804", "repeat_hours": "1"}}
	clock = texelSent.Add(2*time.Hour + time.Minute)
	svc.Repeat(ctx)
	if n := len(bus.published()); n != 4 {
		t.Fatalf("repeat_hours 1 at +2h: %d sends", n)
	}
	for _, bad := range []string{"-1", "169", "six", "1.5"} {
		if _, err := ParseTenantConfig(account(map[string]string{"enabled": "true", "repeat_hours": bad})); err == nil {
			t.Errorf("repeat_hours %q accepted", bad)
		}
	}
	if cfg, _ := ParseTenantConfig(account(map[string]string{"enabled": "true"})); cfg.RepeatHours != DefaultRepeatHours {
		t.Fatalf("default: %d", cfg.RepeatHours)
	}
	if cfg, _ := ParseTenantConfig(account(map[string]string{"enabled": "true", "repeat_hours": " 24 "})); cfg.RepeatHours != 24 {
		t.Fatalf("24: %d", cfg.RepeatHours)
	}
}

// capDoc is a minimal test CAP message: an Alert, or an Update that names
// an earlier identifier.
func capDoc(id, msgType, refs string) []byte {
	return capDocArea(id, msgType, refs, `<area><areaDesc>Texel</areaDesc><geocode><valueName>EMMA_ID</valueName><value>NL804</value></geocode></area>`)
}

// capDocArea is capDoc with the <area> given, "" for none: CAP lets an
// Update or Cancel stand on its references alone.
func capDocArea(id, msgType, refs, area string) []byte {
	references := ""
	if refs != "" {
		references = "<references>" + refs + "</references>"
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2">
  <identifier>%s</identifier><sender>owner@example.org</sender><sent>2026-10-28T12:00:00+00:00</sent>
  <status>Test</status><msgType>%s</msgType><scope>Public</scope>%s
  <info><language>nl</language><category>Met</category><event>Test</event><urgency>Immediate</urgency><severity>Severe</severity><certainty>Observed</certainty>
  <expires>2026-10-28T20:00:00+00:00</expires><senderName>MeshSat</senderName><headline>Relay test %s</headline>
  %s</info></alert>`,
		id, msgType, references, msgType, area))
}

func TestAnUpdateSupersedesTheMessageItNamesAndIsRepeatedInItsPlace(t *testing.T) {
	svc, bus, db, _ := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804", "repeat_hours": "1"}})
	ctx := context.Background()
	sent := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	clock := sent.Add(time.Minute)
	svc.now = func() time.Time { return clock }

	if _, err := svc.Inject(ctx, "t1", capDoc("w-1", "Alert", "")); err != nil {
		t.Fatal(err)
	}
	clock = sent.Add(time.Hour + time.Minute)
	svc.Repeat(ctx)
	if n := len(bus.published()); n != 4 {
		t.Fatalf("the alert and its first repeat: %d sends", n)
	}
	// The Update names w-1: w-1 is superseded, the Update goes out and is
	// what repeats from here.
	if _, err := svc.Inject(ctx, "t1", capDoc("w-2", "Update", "owner@example.org,w-1,2026-10-28T12:00:00+00:00")); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.GetWarning(ctx, "test", "w-1"); w == nil || w.SupersededBy != "w-2" {
		t.Fatalf("w-1 not superseded: %+v", w)
	}
	clock = sent.Add(2*time.Hour + time.Minute)
	svc.Repeat(ctx)
	got := bus.published()
	if len(got) != 8 {
		t.Fatalf("update + its repeat: %d sends", len(got))
	}
	for _, s := range got[6:] {
		d, err := Verify(svc.PublicKey(), s.payload)
		if err != nil || d.ID != IDOf("w-2") || d.MsgType != TypeUpdate || d.Ref != IDOf("w-1") {
			t.Fatalf("the repeat after the update is not the update: %+v %v", d, err)
		}
	}
	// Both are listed; the console can see w-1 was superseded by w-2.
	list, _ := db.ListTenantWarnings(ctx, "t1", 10)
	if len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
	for _, w := range list {
		if w.Identifier == "w-1" && w.SupersededBy != "w-2" {
			t.Fatalf("w-1 in the listing: %+v", w)
		}
	}
}

func TestRepeatsStayOffSatelliteAndOffPhones(t *testing.T) {
	doc := fixture(t, "meteoalarm-nl-texel.cap.xml")
	a, _ := ParseCAP(doc)
	feed := &fakeFeed{source: SourceMeteoAlarmNL, msgs: []Fetched{{Alert: a, Raw: doc}}}
	svc, bus, _, sats := rig(t, fakeAccounts{"t1": {"enabled": "true", "areas": "NL804", "satellite": "true", "repeat_hours": "2"}}, feed)
	ctx := context.Background()
	clock := texelSent.Add(time.Minute)
	svc.now = func() time.Time { return clock }
	svc.Poll(ctx)
	clock = texelSent.Add(2*time.Hour + time.Minute)
	svc.Repeat(ctx)
	clock = texelSent.Add(4*time.Hour + time.Minute)
	svc.Repeat(ctx)
	if len(*sats) != 1 {
		t.Fatalf("satellite sends: %d, want the first delivery only", len(*sats))
	}
	if n := len(bus.published()); n != 6 {
		t.Fatalf("mqtt sends: %d", n)
	}
	for _, s := range bus.published() {
		if strings.Contains(s.topic, "msa-flaneur") {
			t.Fatalf("a repeat went to the phone: %s", s.topic)
		}
	}
}
