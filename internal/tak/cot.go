package tak

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
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

// BuildPositionEventTyped creates a CoT PLI event with a specific CoT type.
// If cotType is empty, defaults to TypePosition (a-f-G-U-C).
func BuildPositionEventTyped(uid, callsign string, lat, lon, alt float64, staleSec int, source, cotType string) CotEvent {
	ev := BuildPositionEvent(uid, callsign, lat, lon, alt, staleSec, source)
	if cotType != "" {
		ev.Type = cotType
	}
	return ev
}

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

// BuildBridgeEvent creates a CoT PLI event for a MeshSat bridge.
func BuildBridgeEvent(birth protocol.BridgeBirth, staleSec int) CotEvent {
	now := time.Now().UTC()

	cotType := birth.CoTType
	if cotType == "" {
		cotType = protocol.CoTBridge
	}

	callsign := birth.CoTCallsign
	if callsign == "" {
		callsign = "BRIDGE-" + birth.BridgeID
	}

	var lat, lon, alt float64
	if birth.Location != nil {
		lat = birth.Location.Lat
		lon = birth.Location.Lon
		alt = birth.Location.Alt
	}

	ce := 10.0
	geoSrc := "fixed"
	if birth.Location != nil && birth.Location.Source != "" {
		geoSrc = birth.Location.Source
	}

	// Build remarks with bridge metadata.
	var parts []string
	if birth.Version != "" {
		parts = append(parts, "v"+birth.Version)
	}
	if birth.Hostname != "" {
		parts = append(parts, "host="+birth.Hostname)
	}
	if birth.Mode != "" {
		parts = append(parts, "mode="+birth.Mode)
	}
	if len(birth.Interfaces) > 0 {
		var ifNames []string
		for _, iface := range birth.Interfaces {
			ifNames = append(ifNames, iface.Name+"("+iface.Status+")")
		}
		parts = append(parts, "if=["+strings.Join(ifNames, ",")+"]")
	}
	if birth.UptimeSec > 0 {
		parts = append(parts, fmt.Sprintf("up=%ds", birth.UptimeSec))
	}
	remarks := strings.Join(parts, " ")

	uid := fmt.Sprintf("meshsat-bridge-%s", birth.BridgeID)

	return CotEvent{
		Version: "2.0",
		UID:     uid,
		Type:    cotType,
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
			Remarks:   &CotRemarks{Source: "MeshSat-Hub", Text: remarks},
		},
	}
}

// BuildDeviceBirthEvent creates a CoT PLI event for a device under a bridge.
func BuildDeviceBirthEvent(device protocol.DeviceBirth, staleSec int) CotEvent {
	now := time.Now().UTC()

	cotType := device.CoTType
	if cotType == "" {
		cotType = protocol.CoTTypeForDevice(device.Type)
	}

	callsign := device.CoTCallsign
	if callsign == "" {
		callsign = device.DeviceID
	}

	var lat, lon, alt float64
	ce := 100.0 // no GPS by default
	geoSrc := "estimated"
	if device.Position != nil {
		lat = device.Position.Lat
		lon = device.Position.Lon
		alt = device.Position.Alt
		ce = 10.0
		geoSrc = "GPS"
		if device.Position.Source != "" {
			geoSrc = device.Position.Source
		}
	}

	// Build remarks with device metadata.
	var parts []string
	if device.Type != "" {
		parts = append(parts, "type="+device.Type)
	}
	if device.Hardware != "" {
		parts = append(parts, "hw="+device.Hardware)
	}
	if device.Firmware != "" {
		parts = append(parts, "fw="+device.Firmware)
	}
	if device.BridgeID != "" {
		parts = append(parts, "bridge="+device.BridgeID)
	}
	remarks := strings.Join(parts, " ")

	uid := fmt.Sprintf("meshsat-device-%s", device.DeviceID)

	return CotEvent{
		Version: "2.0",
		UID:     uid,
		Type:    cotType,
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
			Remarks:   &CotRemarks{Source: "MeshSat-Hub", Text: remarks},
		},
	}
}

// BuildBridgeHealthEvent creates a CoT PLI event from a bridge health update.
// This refreshes the bridge's position on the TAK map. If the birth certificate
// is cached, its location/callsign/CoT type are used; otherwise defaults apply.
func BuildBridgeHealthEvent(health protocol.BridgeHealth, bridgeBirth *protocol.BridgeBirth, staleSec int) CotEvent {
	now := time.Now().UTC()

	cotType := protocol.CoTBridge
	callsign := "BRIDGE-" + health.BridgeID
	var lat, lon, alt float64
	geoSrc := "fixed"

	if bridgeBirth != nil {
		if bridgeBirth.CoTType != "" {
			cotType = bridgeBirth.CoTType
		}
		if bridgeBirth.CoTCallsign != "" {
			callsign = bridgeBirth.CoTCallsign
		}
		if bridgeBirth.Location != nil {
			lat = bridgeBirth.Location.Lat
			lon = bridgeBirth.Location.Lon
			alt = bridgeBirth.Location.Alt
			if bridgeBirth.Location.Source != "" {
				geoSrc = bridgeBirth.Location.Source
			}
		}
	}

	// Build remarks with health metrics.
	var parts []string
	parts = append(parts, fmt.Sprintf("cpu=%.0f%%", health.CPUPct))
	parts = append(parts, fmt.Sprintf("mem=%.0f%%", health.MemPct))
	parts = append(parts, fmt.Sprintf("disk=%.0f%%", health.DiskPct))
	if len(health.Interfaces) > 0 {
		var ifParts []string
		for _, iface := range health.Interfaces {
			ifParts = append(ifParts, iface.Name+"("+iface.Status+")")
		}
		parts = append(parts, "if=["+strings.Join(ifParts, ",")+"]")
	}
	if health.UptimeSec > 0 {
		parts = append(parts, fmt.Sprintf("up=%ds", health.UptimeSec))
	}
	remarks := strings.Join(parts, " ")

	uid := fmt.Sprintf("meshsat-bridge-%s", health.BridgeID)

	return CotEvent{
		Version: "2.0",
		UID:     uid,
		Type:    cotType,
		How:     "m-g",
		Time:    now.Format(cotTimeFormat),
		Start:   now.Format(cotTimeFormat),
		Stale:   now.Add(time.Duration(staleSec) * time.Second).Format(cotTimeFormat),
		Point:   CotPoint{Lat: lat, Lon: lon, Hae: alt, Ce: 10, Le: 10},
		Detail: &CotDetail{
			Contact:   &CotContact{Callsign: callsign},
			Group:     &CotGroup{Name: "Cyan", Role: "Team Member"},
			Precision: &CotPrecision{AltSrc: geoSrc, GeoPointSrc: geoSrc},
			Track:     &CotTrack{Course: 0, Speed: 0},
			Remarks:   &CotRemarks{Source: "MeshSat-Hub", Text: remarks},
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
