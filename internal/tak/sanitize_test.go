package tak

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"
)

// The clock the fixtures are written against.
var sanNow = time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

const (
	sanTimes = `time="2026-10-01T20:00:00Z" start="2026-10-01T20:00:00Z" stale="2026-10-01T20:02:00Z"`
	sanPoint = `<point lat="52.1" lon="4.5" hae="0" ce="10" le="10"/>`
)

// pli is a position event in the shape the Bridge and the apps export.
func pli(uid string) string {
	return `<event version="2.0" uid="` + uid + `" type="a-f-G-U-C" how="m-g" ` + sanTimes + `>` +
		sanPoint +
		`<detail><contact callsign="KIT-1A2B"/><__group name="Cyan" role="Team Member"/></detail></event>`
}

func mustSanitize(t *testing.T, raw string) ([]byte, Meta) {
	t.Helper()
	out, meta, err := Sanitize([]byte(raw), ClientLimits, sanNow)
	if err != nil {
		t.Fatalf("refused: %v\n%s", err, raw)
	}
	return out, meta
}

func reasonOf(t *testing.T, raw string, lim Limits) string {
	t.Helper()
	out, _, err := Sanitize([]byte(raw), lim, sanNow)
	if err == nil {
		t.Fatalf("accepted, and wrote:\n%s\nfrom:\n%s", out, raw)
	}
	var r *Refused
	if !errors.As(err, &r) {
		t.Fatalf("the refusal is not a *Refused, so it has no reason to count: %v", err)
	}
	return r.Reason
}

// What the clients send today goes through, and says what it is.
func TestSanitizeAcceptsAClientsPositionEvent(t *testing.T) {
	out, meta := mustSanitize(t, pli("MESHSAT-aabbccdd"))
	if meta.UID != "MESHSAT-aabbccdd" || meta.Type != "a-f-G-U-C" {
		t.Errorf("meta = %+v", meta)
	}
	if meta.Lat != 52.1 || meta.Lon != 4.5 {
		t.Errorf("the point was read as %v,%v", meta.Lat, meta.Lon)
	}
	if meta.Emergency {
		t.Error("a position update was marked as an emergency")
	}
	if !meta.Stale.Equal(time.Date(2026, 10, 1, 20, 2, 0, 0, time.UTC)) {
		t.Errorf("stale = %v", meta.Stale)
	}
	// It still parses as the event it was.
	var ev CotEvent
	if err := xml.Unmarshal(out, &ev); err != nil {
		t.Fatalf("the output does not parse: %v\n%s", err, out)
	}
	if ev.UID != "MESHSAT-aabbccdd" || ev.Detail == nil || ev.Detail.Contact == nil || ev.Detail.Contact.Callsign != "KIT-1A2B" {
		t.Errorf("the event changed on the way through:\n%s", out)
	}
}

// The reason this is not a re-marshal through CotEvent: that struct knows seven
// detail elements, and a chat message is made of ones it does not know.
func TestSanitizeKeepsUnknownDetailElements(t *testing.T) {
	chat := `<event version="2.0" uid="GeoChat.MESHSAT-aabbccdd.All Chat Rooms.5f3c" type="b-t-f" how="h-g-i-g-o" ` + sanTimes + `>` +
		sanPoint +
		`<detail>` +
		`<__chat parent="RootContactGroup" groupOwner="false" chatroom="All Chat Rooms" id="All Chat Rooms" senderCallsign="KIT-1A2B">` +
		`<chatgrp uid0="MESHSAT-aabbccdd" uid1="All Chat Rooms" id="All Chat Rooms"/></__chat>` +
		`<link uid="MESHSAT-aabbccdd" type="a-f-G-U-C" relation="p-p"/>` +
		`<remarks source="BAO.F.ATAK.MESHSAT-aabbccdd" to="All Chat Rooms" time="2026-10-01T20:00:00Z">at the ridge, all "ok" &amp; moving</remarks>` +
		`<takv device="MeshSat" platform="Bridge" os="linux" version="1.0"/>` +
		`</detail></event>`
	out, _ := mustSanitize(t, chat)
	for _, want := range []string{
		`<__chat parent="RootContactGroup"`, `chatroom="All Chat Rooms"`,
		`<chatgrp uid0="MESHSAT-aabbccdd" uid1="All Chat Rooms" id="All Chat Rooms"/>`,
		`<link uid="MESHSAT-aabbccdd" type="a-f-G-U-C" relation="p-p"/>`,
		`<takv device="MeshSat"`,
		`at the ridge, all &#34;ok&#34; &amp; moving</remarks>`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the output lost %s\n%s", want, out)
		}
	}
}

// One line, one event, nothing a framer could split on.
func TestSanitizeWritesExactlyOneEventOnOneLine(t *testing.T) {
	pretty := "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\n" +
		"<event version=\"2.0\" uid=\"MESHSAT-1\" type=\"a-f-G-U-C\" how=\"m-g\"\n   " + sanTimes + ">\n" +
		"  " + sanPoint + "\n" +
		"  <detail>\n    <remarks>line one\nline two\r\n\ttabbed</remarks>\n    <contact callsign=\"A\nB\"/>\n  </detail>\n" +
		"</event>\n\n"
	out, _ := mustSanitize(t, pretty)
	if bytes.ContainsAny(out, "\n\r\t") {
		t.Errorf("the output carries a raw newline, carriage return or tab: a line-framed reader "+
			"would split it there\n%q", out)
	}
	if n := bytes.Count(out, []byte("</event>")); n != 1 || !bytes.HasSuffix(out, []byte("</event>")) {
		t.Errorf("the output has %d closing event tags and must end with the only one:\n%s", n, out)
	}
	if n := bytes.Count(out, []byte("<event")); n != 1 || !bytes.HasPrefix(out, []byte("<event ")) {
		t.Errorf("the output has %d \"<event\" and must start with the only one:\n%s", n, out)
	}
	if bytes.Contains(out, []byte("<?")) {
		t.Errorf("the XML declaration was written into what is a fragment of a stream:\n%s", out)
	}
	// The line breaks inside the text are still there, as references. XML reads
	// "\r\n" as one line break, so that is what comes out.
	if !bytes.Contains(out, []byte("line one&#xA;line two&#xA;&#x9;tabbed")) {
		t.Errorf("the text's own line breaks were lost rather than escaped:\n%s", out)
	}
}

// What comes out goes back in and comes out the same. Without this, an event
// relayed twice (client to Hub, Hub to a server, back to the Hub) could drift.
func TestSanitizeIsIdempotent(t *testing.T) {
	raw := `<event version="2.0" uid="MESHSAT-&quot;q&apos;&amp;&lt;x&#233;" type="a-f-G-U-C" how="m-g" ` + sanTimes + `>` +
		sanPoint + `<detail><remarks>a &lt; b &amp;&amp; c &gt; d&#10;next</remarks></detail></event>`
	first, m1 := mustSanitize(t, raw)
	second, m2, err := Sanitize(first, ClientLimits, sanNow)
	if err != nil {
		t.Fatalf("the Hub's own output was refused on the way back in: %v\n%s", err, first)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
	if m1.UID != m2.UID || m1.UID != `MESHSAT-"q'&<x`+"é" {
		t.Errorf("the uid changed: %q then %q", m1.UID, m2.UID)
	}
}

// An emergency is recognised either way it is spelled, so the caller can keep it
// out from behind routine traffic.
func TestSanitizeMarksAnEmergency(t *testing.T) {
	alarm := strings.Replace(pli("MESHSAT-1-DEADMAN"), `type="a-f-G-U-C"`, `type="b-a-o-tbl"`, 1)
	if _, meta := mustSanitize(t, alarm); !meta.Emergency {
		t.Error("an alarm type (b-a...) was not marked as an emergency")
	}
	sos := strings.Replace(pli("MESHSAT-1"), `<contact callsign="KIT-1A2B"/>`,
		`<contact callsign="KIT-1A2B"/><emergency type="911 Alert">KIT-1A2B</emergency>`, 1)
	if _, meta := mustSanitize(t, sos); !meta.Emergency {
		t.Error("an event carrying <emergency> was not marked as an emergency")
	}
}

// Every way a payload can be more, or less, than exactly one event.
func TestSanitizeRefusesWhatIsNotExactlyOneEvent(t *testing.T) {
	one := pli("MESHSAT-1")
	for name, c := range map[string]struct{ raw, reason string }{
		"empty":                    {"", ReasonSize},
		"only whitespace":          {" \n ", ReasonStructure},
		"two events":               {one + one, ReasonStructure},
		"two events on two lines":  {one + "\n" + one, ReasonStructure},
		"trailing text":            {one + "x", ReasonStructure},
		"trailing element":         {one + "<a/>", ReasonStructure},
		"leading text":             {"x" + one, ReasonStructure},
		"unterminated":             {strings.TrimSuffix(one, "</event>"), ReasonStructure},
		"cut mid-tag":              {one[:40], ReasonStructure},
		"mismatched tags":          {strings.Replace(one, "</detail>", "</point>", 1), ReasonStructure},
		"stray close":              {"</event>" + one, ReasonStructure},
		"root is not event":        {strings.NewReplacer("<event ", "<evnt ", "</event>", "</evnt>").Replace(one), ReasonStructure},
		"nested event":             {strings.Replace(one, "<detail>", "<detail><event/>", 1), ReasonStructure},
		"an element named eventX":  {strings.Replace(one, "<detail>", "<detail><eventful/>", 1), ReasonStructure},
		"json, as the route sends": {`{"device_id":"300434","text":"hello"}`, ReasonStructure},
		"doctype":                  {`<!DOCTYPE event [<!ENTITY x "y">]>` + one, ReasonMarkup},
		"entity declaration":       {`<!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;">]>` + one, ReasonMarkup},
		"comment":                  {strings.Replace(one, "<detail>", "<detail><!-- hi -->", 1), ReasonMarkup},
		"cdata":                    {strings.Replace(one, "<detail>", "<detail><remarks><![CDATA[</event><event>]]></remarks>", 1), ReasonMarkup},
		"processing instruction":   {strings.Replace(one, "<detail>", "<detail><?php x ?>", 1), ReasonMarkup},
		"a second xml declaration": {`<?xml version="1.0"?><?xml version="1.0"?>` + one, ReasonMarkup},
		"xml-stylesheet up front":  {`<?xml-stylesheet href="x"?>` + one, ReasonMarkup},
		"undefined entity":         {strings.Replace(one, `callsign="KIT-1A2B"`, `callsign="&nope;"`, 1), ReasonStructure},
		"latin-1 declared":         {`<?xml version="1.0" encoding="ISO-8859-1"?>` + one, ReasonEncoding},
		"not utf-8":                {strings.Replace(one, "KIT-1A2B", "KIT-\xff\xfe", 1), ReasonEncoding},
		"a protobuf frame":         {"\xbf\x01\xbf\x0a\x12", ReasonEncoding},
		"prefixed element":         {strings.Replace(one, "<detail>", "<detail><x:y/>", 1), ReasonNamespace},
		"prefixed attribute":       {strings.Replace(one, `<contact `, `<contact xsi:type="a" `, 1), ReasonNamespace},
		"xmlns":                    {strings.Replace(one, `<contact `, `<contact xmlns="urn:x" `, 1), ReasonNamespace},
		"xmlns prefix declared":    {strings.Replace(one, `<contact `, `<contact xmlns:x="urn:x" `, 1), ReasonNamespace},
		"attribute twice":          {strings.Replace(one, `<contact callsign="KIT-1A2B"`, `<contact callsign="A" callsign="B"`, 1), ReasonStructure},
		"text in the event":        {strings.Replace(one, "<detail>", "loose<detail>", 1), ReasonStructure},
		"text in the point":        {strings.Replace(one, sanPoint, `<point lat="52.1" lon="4.5">x</point>`, 1), ReasonStructure},
		"element in the point":     {strings.Replace(one, sanPoint, `<point lat="52.1" lon="4.5"><a/></point>`, 1), ReasonStructure},
		"a third child":            {strings.Replace(one, "<detail>", "<extra/><detail>", 1), ReasonStructure},
		"two points":               {strings.Replace(one, sanPoint, sanPoint+sanPoint, 1), ReasonStructure},
		"two details":              {strings.Replace(one, "<detail>", "<detail/><detail>", 1), ReasonStructure},
	} {
		if got := reasonOf(t, c.raw, ClientLimits); got != c.reason {
			t.Errorf("%s: refused as %q, want %q", name, got, c.reason)
		}
	}
}

// The fields a map needs. An event without them is refused rather than drawn at
// 0,0 or with no name.
func TestSanitizeRefusesAnEventWithoutItsFields(t *testing.T) {
	one := pli("MESHSAT-1")
	for name, c := range map[string]struct{ raw, reason string }{
		"no uid":           {strings.Replace(one, ` uid="MESHSAT-1"`, "", 1), ReasonFields},
		"empty uid":        {strings.Replace(one, `uid="MESHSAT-1"`, `uid=""`, 1), ReasonFields},
		"uid with control": {strings.Replace(one, `uid="MESHSAT-1"`, `uid="a&#10;b"`, 1), ReasonFields},
		"uid too long":     {strings.Replace(one, `uid="MESHSAT-1"`, `uid="`+strings.Repeat("u", 256)+`"`, 1), ReasonFields},
		"no type":          {strings.Replace(one, ` type="a-f-G-U-C"`, "", 1), ReasonFields},
		"type with spaces": {strings.Replace(one, `type="a-f-G-U-C"`, `type="a f"`, 1), ReasonFields},
		"no time":          {strings.Replace(one, ` time="2026-10-01T20:00:00Z"`, "", 1), ReasonFields},
		"time not a time":  {strings.Replace(one, `time="2026-10-01T20:00:00Z"`, `time="yesterday"`, 1), ReasonFields},
		"no stale":         {strings.Replace(one, ` stale="2026-10-01T20:02:00Z"`, "", 1), ReasonFields},
		"no point":         {strings.Replace(one, sanPoint, "", 1), ReasonFields},
		"no lat":           {strings.Replace(one, ` lat="52.1"`, "", 1), ReasonFields},
		"lat not a number": {strings.Replace(one, `lat="52.1"`, `lat="north"`, 1), ReasonFields},
		"lat NaN":          {strings.Replace(one, `lat="52.1"`, `lat="NaN"`, 1), ReasonFields},
		"lat out of range": {strings.Replace(one, `lat="52.1"`, `lat="91"`, 1), ReasonFields},
		"lon out of range": {strings.Replace(one, `lon="4.5"`, `lon="-181"`, 1), ReasonFields},
		"hae infinite":     {strings.Replace(one, `hae="0"`, `hae="+Inf"`, 1), ReasonFields},
		"stale long past":  {strings.Replace(one, `stale="2026-10-01T20:02:00Z"`, `stale="2026-10-01T19:00:00Z"`, 1), ReasonStale},
	} {
		if got := reasonOf(t, c.raw, ClientLimits); got != c.reason {
			t.Errorf("%s: refused as %q, want %q", name, got, c.reason)
		}
	}

	// The skew, from both sides: a sender whose clock is a little behind is not
	// refused, and fractional seconds and offsets are timestamps too.
	late := strings.Replace(one, `stale="2026-10-01T20:02:00Z"`, `stale="2026-10-01T19:56:00.250+00:00"`, 1)
	if _, _, err := Sanitize([]byte(late), ClientLimits, sanNow); err != nil {
		t.Errorf("an event four minutes past stale was refused; the skew is five: %v", err)
	}
}

// The limits hold, and each is the one that says so.
func TestSanitizeHoldsItsLimits(t *testing.T) {
	one := pli("MESHSAT-1")
	lim := ClientLimits

	big := strings.Replace(one, "<detail>", "<detail><remarks>"+strings.Repeat("x", lim.MaxBytes)+"</remarks>", 1)
	if got := reasonOf(t, big, lim); got != ReasonSize {
		t.Errorf("over MaxBytes: %q", got)
	}
	deep := strings.Replace(one, "<detail>", "<detail>"+strings.Repeat("<a>", lim.MaxDepth)+strings.Repeat("</a>", lim.MaxDepth), 1)
	if got := reasonOf(t, deep, lim); got != ReasonLimits {
		t.Errorf("over MaxDepth: %q", got)
	}
	many := strings.Replace(one, "<detail>", "<detail>"+strings.Repeat("<a/>", lim.MaxElements), 1)
	if got := reasonOf(t, many, lim); got != ReasonLimits {
		t.Errorf("over MaxElements: %q", got)
	}
	var attrs strings.Builder
	for i := 0; i <= lim.MaxAttrs; i++ {
		attrs.WriteString(" a")
		attrs.WriteString(strings.Repeat("b", i+1))
		attrs.WriteString(`="1"`)
	}
	wide := strings.Replace(one, "<contact ", "<contact"+attrs.String()+" ", 1)
	if got := reasonOf(t, wide, lim); got != ReasonLimits {
		t.Errorf("over MaxAttrs: %q", got)
	}
	long := strings.Replace(one, `callsign="KIT-1A2B"`, `callsign="`+strings.Repeat("c", lim.MaxAttrLen+1)+`"`, 1)
	if got := reasonOf(t, long, lim); got != ReasonLimits {
		t.Errorf("over MaxAttrLen: %q", got)
	}
	// Escaping lengthens: a raw quote in text is one byte in and five out. A
	// payload inside the input limit whose output would be more than twice it is
	// refused, so a small input cannot be made into a large write.
	quotes := strings.Replace(one, "<detail>", "<detail><remarks>"+strings.Repeat(`"`, lim.MaxBytes/2)+"</remarks>", 1)
	if len(quotes) > lim.MaxBytes {
		t.Fatalf("the fixture is %d bytes, over the input limit it is meant to sit inside", len(quotes))
	}
	if got := reasonOf(t, quotes, lim); got != ReasonSize {
		t.Errorf("output over twice MaxBytes: %q", got)
	}

	// The zero Limits refuses everything, so a caller that forgets to pass limits
	// fails closed.
	if _, _, err := Sanitize([]byte(one), Limits{}, sanNow); err == nil {
		t.Error("the zero Limits accepted an event")
	}
}

// The output of Sanitize, for any input it accepts, is one line holding one
// event, parses, and is a fixed point.
func FuzzSanitize(f *testing.F) {
	for _, seed := range []string{
		pli("MESHSAT-1"),
		pli("MESHSAT-1") + pli("MESHSAT-2"),
		`<event><![CDATA[x]]></event>`,
		`<!DOCTYPE a [<!ENTITY b "c">]><event/>`,
		`<event uid="a&#10;b"/>`,
		"<event\n uid='x'>\n</event>\n",
		`{"text":"hello"}`,
		"\xbf\x01\xbf",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, meta, err := Sanitize(raw, ClientLimits, sanNow)
		if err != nil {
			var r *Refused
			if !errors.As(err, &r) || r.Reason == "" {
				t.Fatalf("a refusal without a reason: %v", err)
			}
			return
		}
		if bytes.ContainsAny(out, "\n\r") {
			t.Fatalf("a raw line break in the output: %q", out)
		}
		if bytes.Count(out, []byte("</event>"))+bytes.Count(out, []byte("<event/>")) != 1 {
			t.Fatalf("not exactly one event end: %q", out)
		}
		if bytes.Contains(out, []byte("<!")) || bytes.Contains(out, []byte("<?")) {
			t.Fatalf("markup the scan refuses came out: %q", out)
		}
		if meta.UID == "" || meta.Type == "" {
			t.Fatalf("accepted without a uid or type: %q", out)
		}
		var ev CotEvent
		if err := xml.Unmarshal(out, &ev); err != nil {
			t.Fatalf("the output does not parse: %v\n%q", err, out)
		}
		again, _, err := Sanitize(out, ClientLimits, sanNow)
		if err != nil {
			t.Fatalf("the output was refused on the way back in: %v\n%q", err, out)
		}
		if !bytes.Equal(out, again) {
			t.Fatalf("not a fixed point:\n%q\n%q", out, again)
		}
	})
}
