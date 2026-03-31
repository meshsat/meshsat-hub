package cloudloop

import (
	"encoding/json"
	"time"
)

// LingoMO is the Cloudloop Data MO message format (LingoMO JSON).
// Received via HTTP webhook or MQTT subscriber from Cloudloop Ground Control.
type LingoMO struct {
	ID         string         `json:"id"`
	ReceivedAt LingoTimestamp `json:"receivedAt"`
	Identity   LingoIdentity  `json:"identity"`
	SBD        *LingoSBD      `json:"sbd,omitempty"`
	IMT        *LingoIMT      `json:"imt,omitempty"`
	Cellular   *LingoCellular `json:"cellular,omitempty"`
	Message    string         `json:"message"` // base64-encoded payload
}

// LingoTimestamp is Cloudloop's structured timestamp format.
type LingoTimestamp struct {
	Year   int `json:"year"`
	Month  int `json:"month"`
	Day    int `json:"day"`
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
	Second int `json:"second"`
}

// Time converts the structured timestamp to a Go time.Time.
func (t LingoTimestamp) Time() time.Time {
	return time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second, 0, time.UTC)
}

// LingoIdentity identifies the device/subscriber within Cloudloop.
type LingoIdentity struct {
	AccountID  string           `json:"accountId"`
	Subscriber *LingoSubscriber `json:"subscriber,omitempty"`
	Hardware   *LingoHardware   `json:"hardware,omitempty"`
	ThingID    string           `json:"thingId"`
}

// LingoSubscriber represents the Cloudloop subscriber details.
type LingoSubscriber struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// LingoHardware represents the physical hardware identity.
type LingoHardware struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	IMEI   string `json:"imei"`
	Serial string `json:"serial"`
}

// LingoSBD contains SBD-specific fields for an MO message.
type LingoSBD struct {
	IMEI         string         `json:"imei"`
	CDRReference string         `json:"cdrReference"`
	MOMSN        int            `json:"momsn"`
	MTMSN        int            `json:"mtmsn"`
	SessionAt    LingoTimestamp `json:"sessionAt"`
	Status       string         `json:"status"`
	Location     *LingoLocation `json:"location,omitempty"`
}

// LingoIMT contains IMT-specific fields for an MO message (RockBLOCK 9704).
type LingoIMT struct {
	CMID      string      `json:"cmid"`
	Topic     string      `json:"topic"`     // IMT_TOPIC_PURPLE, PINK, RED, etc.
	MessageID json.Number `json:"messageId"` // number on wire, not string [MESHSAT-447]
	CRCError  bool        `json:"crcError"`
	Size      int         `json:"size"`
}

// LingoCellular contains cellular-specific fields for an MO message.
type LingoCellular struct {
	MCN    string `json:"mcn"`
	MCC    string `json:"mcc"`
	MSISDN string `json:"msisdn"`
	IMEI   string `json:"imei"`
}

// LingoLocation represents the Iridium CEP location fix.
type LingoLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	CEP       float64 `json:"cep"`
}

// ExtractIMEI extracts the IMEI from the best available source in a LingoMO.
// Priority: identity.hardware.imei > sbd.imei > cellular.imei.
func (m *LingoMO) ExtractIMEI() string {
	if m.Identity.Hardware != nil && m.Identity.Hardware.IMEI != "" {
		return m.Identity.Hardware.IMEI
	}
	if m.SBD != nil && m.SBD.IMEI != "" {
		return m.SBD.IMEI
	}
	if m.Cellular != nil && m.Cellular.IMEI != "" {
		return m.Cellular.IMEI
	}
	return ""
}

// TransmitTime returns the best available transmit timestamp.
func (m *LingoMO) TransmitTime() time.Time {
	if m.SBD != nil {
		return m.SBD.SessionAt.Time()
	}
	return m.ReceivedAt.Time()
}

// MOMSN returns the MO message sequence number (SBD only, 0 for IMT/cellular).
func (m *LingoMO) MOMSN() int {
	if m.SBD != nil {
		return m.SBD.MOMSN
	}
	return 0
}

// Location returns the Iridium CEP location if available.
func (m *LingoMO) Location() (lat, lon, cep float64, ok bool) {
	if m.SBD != nil && m.SBD.Location != nil {
		return m.SBD.Location.Latitude, m.SBD.Location.Longitude, m.SBD.Location.CEP, true
	}
	return 0, 0, 0, false
}

// Source returns a human-readable source string for the message origin.
func (m *LingoMO) Source() string {
	if m.IMT != nil {
		return "cloudloop_imt"
	}
	if m.Cellular != nil {
		return "cloudloop_cellular"
	}
	return "cloudloop_sbd"
}
