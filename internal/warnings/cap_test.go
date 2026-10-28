package warnings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MESHSAT-1734. The fixtures are real documents fetched on 28 Oct 2026: a
// MeteoAlarm Netherlands message (KNMI, with a polygon and an EMMA_ID) and
// two DWD messages (WARNCELLID geocodes, German text).

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseMeteoAlarmCAP(t *testing.T) {
	a, err := ParseCAP(fixture(t, "meteoalarm-nl-texel.cap.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Identifier != "2.49.0.0.528.0.NL.20261028101141.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_2" || a.Sender != "noreply@knmi.nl" {
		t.Fatalf("identity: %q %q", a.Identifier, a.Sender)
	}
	if a.MsgType != "Update" || a.Status != "Actual" || a.Scope != "Public" || a.IsTest() {
		t.Fatalf("header: %+v", a)
	}
	if !a.Sent.Equal(time.Date(2026, 10, 28, 10, 11, 41, 0, time.UTC)) {
		t.Fatalf("sent: %v", a.Sent)
	}
	if len(a.References) != 1 || a.References[0].Identifier != "2.49.0.0.528.0.NL.20261028041400.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_1" || a.References[0].Sender != "noreply@knmi.nl" {
		t.Fatalf("references: %+v", a.References)
	}
	if len(a.Infos) != 2 {
		t.Fatalf("infos: %d", len(a.Infos))
	}
	en := a.InfoFor([]string{"nl", "en"})
	if en == nil || en.Language != "nl-NL" {
		t.Fatalf("the Dutch block should win: %+v", en)
	}
	en = a.InfoFor([]string{"en"})
	if en.Language != "en-GB" || en.Event != "Moderate Wind" || en.Severity != "Moderate" || en.Urgency != "Expected" || en.Certainty != "Observed" {
		t.Fatalf("en info: %+v", en)
	}
	if en.Headline != "From Wednesday 28 October onwards: chance of wind" || !strings.HasPrefix(en.Description, "From Wednesday") {
		t.Fatalf("text: %q / %q", en.Headline, en.Description)
	}
	if !en.Expires.Equal(time.Date(2026, 10, 29, 10, 11, 41, 0, time.UTC)) || !a.Expires().Equal(en.Expires) {
		t.Fatalf("expires: %v", en.Expires)
	}
	if len(en.Areas) != 1 || en.Areas[0].Desc != "Texel" || len(en.Areas[0].Polygons) != 1 || len(en.Areas[0].Polygons[0]) < 30 {
		t.Fatalf("area: %+v", en.Areas)
	}
	if got := a.Geocodes(); len(got) != 1 || got[0] != "EMMA_ID:NL804" {
		t.Fatalf("geocodes: %v", got)
	}
	if EventCode(en) != EventWind {
		t.Fatalf("event code: %d", EventCode(en))
	}
	if a.InfoFor(nil).Language != "en-GB" {
		t.Fatal("no preference should take the first block")
	}
}

func TestParseDWDCAP(t *testing.T) {
	a, err := ParseCAP(fixture(t, "dwd-boeen.cap.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if a.MsgType != "Alert" || a.Sender != "opendata@dwd.de" || len(a.References) != 0 {
		t.Fatalf("header: %+v", a)
	}
	info := a.InfoFor([]string{"de"})
	if info.Language != "de-DE" || info.Event != "BÖEN" || info.Severity != "Minor" || info.Urgency != "Immediate" || info.Certainty != "Likely" {
		t.Fatalf("info: %+v", info)
	}
	if info.SenderName != "DWD / Seewetterdienst Hamburg" || !strings.HasPrefix(info.Headline, "Amtliche Warnung") {
		t.Fatalf("sender: %q %q", info.SenderName, info.Headline)
	}
	if len(info.Areas) != 4 || info.Areas[0].Desc != "Nordfriesische Küste" {
		t.Fatalf("areas: %+v", info.Areas)
	}
	got := a.Geocodes()
	if len(got) != 4 || got[0] != "WARNCELLID:501000005" {
		t.Fatalf("geocodes: %v", got)
	}
	if EventCode(info) != EventWind {
		t.Fatalf("event code: %d", EventCode(info))
	}
	if !info.Effective.Equal(time.Date(2026, 10, 28, 11, 43, 0, 0, time.UTC)) {
		t.Fatalf("effective: %v", info.Effective)
	}
	if !a.Expires().IsZero() {
		t.Fatalf("this DWD message carries no expiry: %v", a.Expires())
	}
}

func TestParseCAPRefusals(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"not xml", "hello"},
		{"other root", `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`},
		{"no identifier", `<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2"><sender>x</sender><sent>2026-10-28T10:00:00+00:00</sent></alert>`},
		{"no sent", `<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2"><identifier>a</identifier><sender>x</sender></alert>`},
		{"doctype", `<!DOCTYPE alert [<!ENTITY x "y">]><alert><identifier>a</identifier><sender>x</sender><sent>2026-10-28T10:00:00+00:00</sent></alert>`},
	}
	for _, c := range cases {
		if _, err := ParseCAP([]byte(c.doc)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if _, err := ParseCAP(make([]byte, MaxCAPBytes+1)); err == nil {
		t.Error("an oversized document was accepted")
	}
}

func TestParseAreaShapes(t *testing.T) {
	doc := `<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2"><identifier>a</identifier><sender>x</sender><sent>2026-10-28T10:00:00Z</sent>
	<status>Test</status><msgType>Cancel</msgType><references>x,prev,2026-10-28T09:00:00Z x,older</references>
	<info><area><areaDesc>c</areaDesc><circle>52.1,4.3 10.5</circle><polygon>52,4 53,4 53,5 52,5 52,4</polygon><polygon>bad</polygon>
	<geocode><valueName>EMMA_ID</valueName><value>NL804</value></geocode></area></info></alert>`
	a, err := ParseCAP([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsTest() || a.MsgType != "Cancel" {
		t.Fatalf("header: %+v", a)
	}
	if len(a.References) != 2 || a.References[0].Identifier != "prev" || a.References[0].Sent != "2026-10-28T09:00:00Z" || a.References[1].Identifier != "older" {
		t.Fatalf("references: %+v", a.References)
	}
	ar := a.Infos[0].Areas[0]
	if len(ar.Circles) != 1 || ar.Circles[0].RadiusKm != 10.5 || ar.Circles[0].Centre.Lat != 52.1 {
		t.Fatalf("circle: %+v", ar.Circles)
	}
	// The closing point is dropped; the bad polygon too.
	if len(ar.Polygons) != 1 || len(ar.Polygons[0]) != 4 {
		t.Fatalf("polygons: %+v", ar.Polygons)
	}
	if a.Infos[0].Language != "en-US" {
		t.Fatalf("the CAP default language: %q", a.Infos[0].Language)
	}
}
