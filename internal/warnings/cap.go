// Package warnings relays official public warnings to the kits that are in
// their area (MESHSAT-1734, the last-mile warning relay).
//
// The leader polls the open CAP feeds (DWD, MeteoAlarm), keeps every message
// it sees, matches each new one against the tenants that opted in (an area
// they named, or where their kits are), and hands a signed, compact digest to
// each matching kit on its own {ns}/bridge/{id}/warn/in, and over satellite
// when the tenant pays for that. The kit verifies the digest with the Hub's
// directory key it already pins, and puts the warning on its mesh and its
// APRS bulletin channel. docs/warning-relay.md is the contract.
package warnings

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/geo"
)

// MaxCAPBytes bounds one CAP document: the largest DWD district file seen is
// about 160 KB; a MeteoAlarm message a few KB.
const MaxCAPBytes = 1 << 20

// CAP 1.2 (OASIS), the parts the relay reads. Element names follow the
// standard; everything the relay does not read is ignored by the decoder.

// Alert is one CAP message.
type Alert struct {
	Identifier string
	Sender     string
	Sent       time.Time
	Status     string // Actual, Exercise, System, Test, Draft
	MsgType    string // Alert, Update, Cancel, Ack, Error
	Scope      string // Public, Restricted, Private
	Source     string
	Code       []string
	Note       string
	// References names the earlier messages this one updates or cancels:
	// "sender,identifier,sent" triples separated by spaces.
	References []Reference
	Infos      []Info
}

// Reference is one earlier message named in <references>.
type Reference struct {
	Sender     string
	Identifier string
	Sent       string
}

// Info is one <info> block: the warning in one language.
type Info struct {
	Language     string
	Category     []string
	Event        string
	ResponseType []string
	Urgency      string
	Severity     string
	Certainty    string
	EventCode    []NamedValue
	Effective    time.Time
	Onset        time.Time
	Expires      time.Time
	SenderName   string
	Headline     string
	Description  string
	Instruction  string
	Web          string
	Parameter    []NamedValue
	Areas        []Area
}

// NamedValue is a CAP valueName/value pair (eventCode, parameter, geocode).
type NamedValue struct {
	Name  string
	Value string
}

// Area is one <area>: a description with polygons, circles and geocodes.
type Area struct {
	Desc     string
	Polygons [][]geo.Point
	Circles  []Circle
	Geocodes []NamedValue
}

// Circle is a CAP circle: a centre and a radius in kilometres.
type Circle struct {
	Centre   geo.Point
	RadiusKm float64
}

// The XML shapes. Separate from the public types so the public ones can
// carry parsed times and points.
type capAlert struct {
	XMLName    xml.Name  `xml:"alert"`
	Identifier string    `xml:"identifier"`
	Sender     string    `xml:"sender"`
	Sent       string    `xml:"sent"`
	Status     string    `xml:"status"`
	MsgType    string    `xml:"msgType"`
	Source     string    `xml:"source"`
	Scope      string    `xml:"scope"`
	Code       []string  `xml:"code"`
	Note       string    `xml:"note"`
	References string    `xml:"references"`
	Info       []capInfo `xml:"info"`
}

type capInfo struct {
	Language     string          `xml:"language"`
	Category     []string        `xml:"category"`
	Event        string          `xml:"event"`
	ResponseType []string        `xml:"responseType"`
	Urgency      string          `xml:"urgency"`
	Severity     string          `xml:"severity"`
	Certainty    string          `xml:"certainty"`
	EventCode    []capNamedValue `xml:"eventCode"`
	Effective    string          `xml:"effective"`
	Onset        string          `xml:"onset"`
	Expires      string          `xml:"expires"`
	SenderName   string          `xml:"senderName"`
	Headline     string          `xml:"headline"`
	Description  string          `xml:"description"`
	Instruction  string          `xml:"instruction"`
	Web          string          `xml:"web"`
	Parameter    []capNamedValue `xml:"parameter"`
	Area         []capArea       `xml:"area"`
}

type capNamedValue struct {
	ValueName string `xml:"valueName"`
	Value     string `xml:"value"`
}

type capArea struct {
	AreaDesc string          `xml:"areaDesc"`
	Polygon  []string        `xml:"polygon"`
	Circle   []string        `xml:"circle"`
	Geocode  []capNamedValue `xml:"geocode"`
}

// ErrNotCAP is answered for a document that is not a CAP 1.2 alert.
var ErrNotCAP = errors.New("warnings: not a CAP 1.2 alert")

// ParseCAP reads one CAP 1.2 alert. It refuses a document without an
// identifier, sender or sent time, a DOCTYPE, and anything over MaxCAPBytes.
func ParseCAP(data []byte) (*Alert, error) {
	if len(data) > MaxCAPBytes {
		return nil, fmt.Errorf("warnings: CAP document over %d bytes", MaxCAPBytes)
	}
	if bytes.Contains(data, []byte("<!DOCTYPE")) || bytes.Contains(data, []byte("<!ENTITY")) {
		return nil, errors.New("warnings: CAP document with a DOCTYPE is refused")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	// A document in another charset would need a converter; the feeds are UTF-8.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "", "utf-8", "utf8":
			return input, nil
		}
		return nil, fmt.Errorf("charset %q not supported", charset)
	}
	var raw capAlert
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotCAP, err)
	}
	if raw.XMLName.Local != "alert" {
		return nil, ErrNotCAP
	}
	a := &Alert{
		Identifier: strings.TrimSpace(raw.Identifier),
		Sender:     strings.TrimSpace(raw.Sender),
		Status:     strings.TrimSpace(raw.Status),
		MsgType:    strings.TrimSpace(raw.MsgType),
		Scope:      strings.TrimSpace(raw.Scope),
		Source:     strings.TrimSpace(raw.Source),
		Code:       raw.Code,
		Note:       strings.TrimSpace(raw.Note),
		References: parseReferences(raw.References),
	}
	if a.Identifier == "" || a.Sender == "" {
		return nil, fmt.Errorf("%w: no identifier or sender", ErrNotCAP)
	}
	var err error
	if a.Sent, err = parseCAPTime(raw.Sent); err != nil || a.Sent.IsZero() {
		return nil, fmt.Errorf("%w: sent %q", ErrNotCAP, raw.Sent)
	}
	for _, ri := range raw.Info {
		info := Info{
			Language:     strings.TrimSpace(ri.Language),
			Category:     ri.Category,
			Event:        strings.TrimSpace(ri.Event),
			ResponseType: ri.ResponseType,
			Urgency:      strings.TrimSpace(ri.Urgency),
			Severity:     strings.TrimSpace(ri.Severity),
			Certainty:    strings.TrimSpace(ri.Certainty),
			SenderName:   strings.TrimSpace(ri.SenderName),
			Headline:     strings.TrimSpace(ri.Headline),
			Description:  strings.TrimSpace(ri.Description),
			Instruction:  strings.TrimSpace(ri.Instruction),
			Web:          strings.TrimSpace(ri.Web),
		}
		if info.Language == "" {
			info.Language = "en-US" // the CAP default
		}
		info.Effective, _ = parseCAPTime(ri.Effective)
		info.Onset, _ = parseCAPTime(ri.Onset)
		info.Expires, _ = parseCAPTime(ri.Expires)
		for _, nv := range ri.EventCode {
			info.EventCode = append(info.EventCode, NamedValue{strings.TrimSpace(nv.ValueName), strings.TrimSpace(nv.Value)})
		}
		for _, nv := range ri.Parameter {
			info.Parameter = append(info.Parameter, NamedValue{strings.TrimSpace(nv.ValueName), strings.TrimSpace(nv.Value)})
		}
		for _, ra := range ri.Area {
			area := Area{Desc: strings.TrimSpace(ra.AreaDesc)}
			for _, p := range ra.Polygon {
				if pts := parsePolygon(p); len(pts) >= 3 {
					area.Polygons = append(area.Polygons, pts)
				}
			}
			for _, c := range ra.Circle {
				if circle, ok := parseCircle(c); ok {
					area.Circles = append(area.Circles, circle)
				}
			}
			for _, g := range ra.Geocode {
				if n, v := strings.TrimSpace(g.ValueName), strings.TrimSpace(g.Value); n != "" && v != "" {
					area.Geocodes = append(area.Geocodes, NamedValue{n, v})
				}
			}
			info.Areas = append(info.Areas, area)
		}
		a.Infos = append(a.Infos, info)
	}
	return a, nil
}

// parseCAPTime reads a CAP dateTime (RFC 3339 with a numeric zone; "Z" is
// not allowed by the standard but is accepted here).
func parseCAPTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("not a CAP time: %q", s)
}

// parseReferences splits "sender,identifier,sent sender,identifier,sent".
func parseReferences(s string) []Reference {
	var out []Reference
	for _, f := range strings.Fields(s) {
		parts := strings.SplitN(f, ",", 3)
		if len(parts) < 2 {
			continue
		}
		r := Reference{Sender: parts[0], Identifier: parts[1]}
		if len(parts) == 3 {
			r.Sent = parts[2]
		}
		if r.Identifier != "" {
			out = append(out, r)
		}
	}
	return out
}

// parsePolygon reads "lat,lon lat,lon ..." (CAP: WGS 84, the first and last
// point equal). A point outside the globe drops the polygon.
func parsePolygon(s string) []geo.Point {
	var pts []geo.Point
	for _, f := range strings.Fields(s) {
		p, ok := parsePoint(f)
		if !ok {
			return nil
		}
		pts = append(pts, p)
	}
	if len(pts) >= 2 && pts[0] == pts[len(pts)-1] {
		pts = pts[:len(pts)-1]
	}
	return pts
}

// parseCircle reads "lat,lon radius" with the radius in kilometres.
func parseCircle(s string) (Circle, bool) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return Circle{}, false
	}
	p, ok := parsePoint(f[0])
	if !ok {
		return Circle{}, false
	}
	r, err := strconv.ParseFloat(f[1], 64)
	if err != nil || r < 0 || r > 20000 {
		return Circle{}, false
	}
	return Circle{Centre: p, RadiusKm: r}, true
}

func parsePoint(s string) (geo.Point, bool) {
	ll := strings.SplitN(s, ",", 2)
	if len(ll) != 2 {
		return geo.Point{}, false
	}
	lat, err1 := strconv.ParseFloat(ll[0], 64)
	lon, err2 := strconv.ParseFloat(ll[1], 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return geo.Point{}, false
	}
	return geo.Point{Lat: lat, Lon: lon}, true
}

// IsTest reports whether the message is an exercise or a test, which the
// relay marks as such on every bearer.
func (a *Alert) IsTest() bool {
	return strings.EqualFold(a.Status, "Test") || strings.EqualFold(a.Status, "Exercise")
}

// InfoFor picks the <info> block in one of the wanted languages (ISO 639-1
// prefixes, in order of preference), or the first block when none matches
// or no preference is given. nil for an alert without info (a Cancel may
// carry none).
func (a *Alert) InfoFor(languages []string) *Info {
	if len(a.Infos) == 0 {
		return nil
	}
	for _, want := range languages {
		want = strings.ToLower(strings.TrimSpace(want))
		if want == "" {
			continue
		}
		for i := range a.Infos {
			if lang := strings.ToLower(a.Infos[i].Language); lang == want || strings.HasPrefix(lang, want+"-") {
				return &a.Infos[i]
			}
		}
	}
	return &a.Infos[0]
}

// Expires is the latest expiry over every info block; zero when none says.
func (a *Alert) Expires() time.Time {
	var t time.Time
	for _, i := range a.Infos {
		if i.Expires.After(t) {
			t = i.Expires
		}
	}
	return t
}

// Geocodes lists every geocode of every area of every info block, as
// "NAME:VALUE", without duplicates.
func (a *Alert) Geocodes() []string {
	seen := map[string]bool{}
	var out []string
	for _, i := range a.Infos {
		for _, ar := range i.Areas {
			for _, g := range ar.Geocodes {
				k := g.Name + ":" + g.Value
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
	}
	return out
}
