package tak

import (
	"encoding/xml"
	"fmt"
	"time"
)

// CoT XML structs — ported from meshsat Bridge (internal/gateway/tak_cot.go).

// CotEvent is the CoT XML event envelope.
type CotEvent struct {
	XMLName xml.Name   `xml:"event"`
	Version string     `xml:"version,attr"`
	UID     string     `xml:"uid,attr"`
	Type    string     `xml:"type,attr"`
	How     string     `xml:"how,attr"`
	Time    string     `xml:"time,attr"`
	Start   string     `xml:"start,attr"`
	Stale   string     `xml:"stale,attr"`
	Point   CotPoint   `xml:"point"`
	Detail  *CotDetail `xml:"detail,omitempty"`
}

// CotPoint is the CoT point element with WGS84 coordinates.
type CotPoint struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
	Hae float64 `xml:"hae,attr"`
	Ce  float64 `xml:"ce,attr"`
	Le  float64 `xml:"le,attr"`
}

// CotDetail holds optional detail sub-elements.
type CotDetail struct {
	Contact   *CotContact   `xml:"contact,omitempty"`
	Group     *CotGroup     `xml:"__group,omitempty"`
	Precision *CotPrecision `xml:"precisionlocation,omitempty"`
	Track     *CotTrack     `xml:"track,omitempty"`
	Status    *CotStatus    `xml:"status,omitempty"`
	Emergency *CotEmergency `xml:"emergency,omitempty"`
	Remarks   *CotRemarks   `xml:"remarks,omitempty"`
}

type CotContact struct {
	Callsign string `xml:"callsign,attr"`
}

type CotGroup struct {
	Name string `xml:"name,attr"`
	Role string `xml:"role,attr"`
}

type CotPrecision struct {
	AltSrc      string `xml:"altsrc,attr"`
	GeoPointSrc string `xml:"geopointsrc,attr"`
}

type CotTrack struct {
	Course float64 `xml:"course,attr"`
	Speed  float64 `xml:"speed,attr"`
}

type CotStatus struct {
	Battery string `xml:"battery,attr,omitempty"`
}

type CotEmergency struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type CotRemarks struct {
	Source string `xml:"source,attr,omitempty"`
	Text   string `xml:",chardata"`
}

const cotTimeFormat = "2006-01-02T15:04:05Z"

const (
	TypePosition  = "a-f-G-U-C"
	TypeSensor    = "t-x-d-d"
	TypeAlarm     = "b-a"
	TypeChat      = "b-t-f"
	TypeKeepalive = "t-x-c-t"
)

// BuildPositionEvent creates a CoT PLI event.
func BuildPositionEvent(uid, callsign string, lat, lon, alt float64, staleSec int, source string) CotEvent {
	now := time.Now().UTC()
	ce := 10.0
	geoSrc := "GPS"
	// Iridium CEP is ~10km
	if source == "iridium_cep" {
		ce = 10000.0
		geoSrc = "Iridium-CEP"
	}
	return CotEvent{
		Version: "2.0",
		UID:     uid,
		Type:    TypePosition,
		How:     "m-g",
		Time:    now.Format(cotTimeFormat),
		Start:   now.Format(cotTimeFormat),
		Stale:   now.Add(time.Duration(staleSec) * time.Second).Format(cotTimeFormat),
		Point:   CotPoint{Lat: lat, Lon: lon, Hae: alt, Ce: ce, Le: ce},
		Detail: &CotDetail{
			Contact:   &CotContact{Callsign: callsign},
			Group:     &CotGroup{Name: "Cyan", Role: "Team Member"},
			Precision: &CotPrecision{AltSrc: geoSrc, GeoPointSrc: geoSrc},
			Track:     &CotTrack{Course: 0, Speed: 0},
			Remarks:   &CotRemarks{Source: "MeshSat-Hub", Text: "Via " + source},
		},
	}
}

// BuildSOSEvent creates a CoT emergency event.
func BuildSOSEvent(uid, callsign string, lat, lon float64, staleSec int) CotEvent {
	ev := BuildPositionEvent(uid, callsign, lat, lon, 0, staleSec, "sos")
	ev.Detail.Emergency = &CotEmergency{
		Type: "911 Alert",
		Text: "SOS activated",
	}
	ev.Detail.Remarks = &CotRemarks{
		Source: "MeshSat-Hub",
		Text:   "Emergency: SOS activated on device " + uid,
	}
	return ev
}

// BuildTelemetryEvent creates a CoT data event for sensor telemetry.
func BuildTelemetryEvent(uid, callsign string, lat, lon float64, staleSec int, data string) CotEvent {
	now := time.Now().UTC()
	return CotEvent{
		Version: "2.0",
		UID:     uid + "-SENSOR",
		Type:    TypeSensor,
		How:     "m-g",
		Time:    now.Format(cotTimeFormat),
		Start:   now.Format(cotTimeFormat),
		Stale:   now.Add(time.Duration(staleSec) * time.Second).Format(cotTimeFormat),
		Point:   CotPoint{Lat: lat, Lon: lon, Hae: 0, Ce: 50, Le: 50},
		Detail: &CotDetail{
			Contact: &CotContact{Callsign: callsign + "-SENSOR"},
			Remarks: &CotRemarks{Source: "MeshSat-Hub", Text: data},
		},
	}
}

// BuildChatEvent creates a CoT GeoChat event.
func BuildChatEvent(uid, callsign, text string, staleSec int) CotEvent {
	now := time.Now().UTC()
	return CotEvent{
		Version: "2.0",
		UID:     fmt.Sprintf("%s-CHAT-%d", uid, now.UnixMilli()),
		Type:    TypeChat,
		How:     "h-g-i-g-o",
		Time:    now.Format(cotTimeFormat),
		Start:   now.Format(cotTimeFormat),
		Stale:   now.Add(time.Duration(staleSec) * time.Second).Format(cotTimeFormat),
		Point:   CotPoint{Lat: 0, Lon: 0, Hae: 0, Ce: 9999999, Le: 9999999},
		Detail: &CotDetail{
			Contact: &CotContact{Callsign: callsign},
			Remarks: &CotRemarks{Source: callsign, Text: text},
		},
	}
}

// MarshalCotEvent serializes a CoT event to XML bytes.
func MarshalCotEvent(ev CotEvent) ([]byte, error) {
	return xml.Marshal(ev)
}

// ParseCotEvent deserializes CoT XML into a CotEvent.
func ParseCotEvent(data []byte) (*CotEvent, error) {
	var ev CotEvent
	if err := xml.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("parse cot: %w", err)
	}
	return &ev, nil
}
