package geo

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// Compact GPS binary codec for field devices.
//
// Wire format (20 bytes):
//
//	Byte 0:    magic 0xA5 (identifies a GPS position frame)
//	Byte 1:    flags [bit0: hasAlt, bit1: hasSpeed, bit2: hasHeading, bit3: hasSats]
//	Bytes 2-5: latitude  as int32 (degrees × 1e7)
//	Bytes 6-9: longitude as int32 (degrees × 1e7)
//	Bytes 10-11: altitude as int16 (meters, signed, -32768..32767), if hasAlt
//	Bytes 12-13: speed as uint16 (cm/s, 0..655.35 m/s), if hasSpeed
//	Bytes 14-15: heading as uint16 (degrees × 100, 0..36000), if hasHeading
//	Byte 16:   sats as uint8, if hasSats
//	Bytes 17-20: timestamp as uint32 (unix epoch, seconds)
//
// Fields after flags are packed — absent fields are omitted to save SBD bytes.
// The total length depends on which flags are set (min 10 bytes, max 21 bytes).

const (
	gpsMagic       byte = 0xA5
	flagHasAlt     byte = 1 << 0
	flagHasSpeed   byte = 1 << 1
	flagHasHeading byte = 1 << 2
	flagHasSats    byte = 1 << 3
)

// GPSPosition represents a decoded GPS position from the compact binary codec.
type GPSPosition struct {
	Lat        float64
	Lon        float64
	Alt        float64
	Speed      float64 // m/s
	Heading    float64 // degrees 0-360
	Sats       int
	Timestamp  time.Time
	HasAlt     bool
	HasSpeed   bool
	HasHeading bool
	HasSats    bool
}

// ErrNotGPSFrame is returned when the magic byte doesn't match.
var ErrNotGPSFrame = errors.New("geo: not a GPS frame (bad magic byte)")

// ErrTooShort is returned when the payload is shorter than the minimum frame.
var ErrTooShort = errors.New("geo: payload too short for GPS frame")

// IsGPSFrame returns true if the payload starts with the GPS magic byte.
func IsGPSFrame(data []byte) bool {
	return len(data) >= 2 && data[0] == gpsMagic
}

// DecodeGPS decodes a compact binary GPS frame into a GPSPosition.
func DecodeGPS(data []byte) (*GPSPosition, error) {
	if len(data) < 2 {
		return nil, ErrTooShort
	}
	if data[0] != gpsMagic {
		return nil, ErrNotGPSFrame
	}

	flags := data[1]
	pos := 2

	// Latitude (4 bytes, required)
	if pos+4 > len(data) {
		return nil, ErrTooShort
	}
	latRaw := int32(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4

	// Longitude (4 bytes, required)
	if pos+4 > len(data) {
		return nil, ErrTooShort
	}
	lonRaw := int32(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4

	p := &GPSPosition{
		Lat: float64(latRaw) / 1e7,
		Lon: float64(lonRaw) / 1e7,
	}

	if flags&flagHasAlt != 0 {
		if pos+2 > len(data) {
			return nil, ErrTooShort
		}
		altRaw := int16(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		p.Alt = float64(altRaw)
		p.HasAlt = true
	}

	if flags&flagHasSpeed != 0 {
		if pos+2 > len(data) {
			return nil, ErrTooShort
		}
		speedRaw := binary.BigEndian.Uint16(data[pos : pos+2])
		pos += 2
		p.Speed = float64(speedRaw) / 100.0
		p.HasSpeed = true
	}

	if flags&flagHasHeading != 0 {
		if pos+2 > len(data) {
			return nil, ErrTooShort
		}
		headingRaw := binary.BigEndian.Uint16(data[pos : pos+2])
		pos += 2
		p.Heading = float64(headingRaw) / 100.0
		p.HasHeading = true
	}

	if flags&flagHasSats != 0 {
		if pos+1 > len(data) {
			return nil, ErrTooShort
		}
		p.Sats = int(data[pos])
		pos++
		p.HasSats = true
	}

	// Timestamp (4 bytes, optional — if remaining)
	if pos+4 <= len(data) {
		ts := binary.BigEndian.Uint32(data[pos : pos+4])
		p.Timestamp = time.Unix(int64(ts), 0).UTC()
	}

	return p, nil
}

// --- Bridge/Android format (0x50 full, 0x44 delta) ---
//
// Bridge and Android devices use a different wire format:
//   Full (0x50, 16 bytes): [magic][lat:i32 LE microdeg][lon:i32 LE microdeg][alt:i16 LE m][hdg:u16 LE deg][spd:u16 LE cm/s][bat:u8 %]
//   Delta (0x44, 11 bytes): [magic][dlat:i16 LE microdeg][dlon:i16 LE microdeg][dalt:i8 m][hdg:u16 LE deg][spd:u16 LE cm/s][bat:u8 %]

const (
	bridgeFullMagic   byte = 0x50
	bridgeDeltaMagic  byte = 0x44
	bridgeFullSize         = 16
	bridgeDeltaSize        = 11
	bridgeLatLonScale      = 1e6
)

// IsBridgeGPSFrame returns true if the payload starts with a bridge full (0x50) or delta (0x44) magic byte.
func IsBridgeGPSFrame(data []byte) bool {
	if len(data) < 1 {
		return false
	}
	return data[0] == bridgeFullMagic || data[0] == bridgeDeltaMagic
}

// DecodeBridgeGPSFull decodes a bridge/Android full position frame (0x50, 16 bytes, LE, microdegrees).
func DecodeBridgeGPSFull(data []byte) (*GPSPosition, error) {
	if len(data) < bridgeFullSize {
		return nil, ErrTooShort
	}
	if data[0] != bridgeFullMagic {
		return nil, ErrNotGPSFrame
	}

	lat := int32(binary.LittleEndian.Uint32(data[1:5]))
	lon := int32(binary.LittleEndian.Uint32(data[5:9]))
	alt := int16(binary.LittleEndian.Uint16(data[9:11]))
	hdg := binary.LittleEndian.Uint16(data[11:13])
	spd := binary.LittleEndian.Uint16(data[13:15])

	return &GPSPosition{
		Lat:        float64(lat) / bridgeLatLonScale,
		Lon:        float64(lon) / bridgeLatLonScale,
		Alt:        float64(alt),
		HasAlt:     true,
		Heading:    float64(hdg),
		HasHeading: true,
		Speed:      float64(spd) / 100.0,
		HasSpeed:   true,
	}, nil
}

// DecodeBridgeGPSDelta decodes a bridge/Android delta position frame (0x44, 11 bytes, LE).
// It requires the previous absolute position to compute absolute coordinates.
func DecodeBridgeGPSDelta(data []byte, prev *GPSPosition) (*GPSPosition, error) {
	if len(data) < bridgeDeltaSize {
		return nil, ErrTooShort
	}
	if data[0] != bridgeDeltaMagic {
		return nil, ErrNotGPSFrame
	}
	if prev == nil {
		return nil, errors.New("geo: delta frame requires previous position")
	}

	dlat := int16(binary.LittleEndian.Uint16(data[1:3]))
	dlon := int16(binary.LittleEndian.Uint16(data[3:5]))
	dalt := int8(data[5])
	hdg := binary.LittleEndian.Uint16(data[6:8])
	spd := binary.LittleEndian.Uint16(data[8:10])

	prevLatMicro := int32(math.Round(prev.Lat * bridgeLatLonScale))
	prevLonMicro := int32(math.Round(prev.Lon * bridgeLatLonScale))

	return &GPSPosition{
		Lat:        float64(prevLatMicro+int32(dlat)) / bridgeLatLonScale,
		Lon:        float64(prevLonMicro+int32(dlon)) / bridgeLatLonScale,
		Alt:        prev.Alt + float64(dalt),
		HasAlt:     true,
		Heading:    float64(hdg),
		HasHeading: true,
		Speed:      float64(spd) / 100.0,
		HasSpeed:   true,
	}, nil
}

// IsAnyGPSFrame returns true if the payload starts with any known GPS magic byte (0xA5, 0x50, 0x44).
func IsAnyGPSFrame(data []byte) bool {
	return IsGPSFrame(data) || IsBridgeGPSFrame(data)
}

// EncodeGPS encodes a GPSPosition into the compact binary format.
func EncodeGPS(p *GPSPosition) []byte {
	var flags byte
	if p.HasAlt || p.Alt != 0 {
		flags |= flagHasAlt
	}
	if p.HasSpeed || p.Speed != 0 {
		flags |= flagHasSpeed
	}
	if p.HasHeading || p.Heading != 0 {
		flags |= flagHasHeading
	}
	if p.HasSats || p.Sats != 0 {
		flags |= flagHasSats
	}

	buf := make([]byte, 0, 21)
	buf = append(buf, gpsMagic, flags)

	// Lat/Lon as int32 × 1e7
	latRaw := int32(math.Round(p.Lat * 1e7))
	lonRaw := int32(math.Round(p.Lon * 1e7))
	buf = binary.BigEndian.AppendUint32(buf, uint32(latRaw))
	buf = binary.BigEndian.AppendUint32(buf, uint32(lonRaw))

	if flags&flagHasAlt != 0 {
		altRaw := int16(math.Round(p.Alt))
		buf = binary.BigEndian.AppendUint16(buf, uint16(altRaw))
	}
	if flags&flagHasSpeed != 0 {
		speedRaw := uint16(math.Round(p.Speed * 100))
		buf = binary.BigEndian.AppendUint16(buf, speedRaw)
	}
	if flags&flagHasHeading != 0 {
		headingRaw := uint16(math.Round(p.Heading * 100))
		buf = binary.BigEndian.AppendUint16(buf, headingRaw)
	}
	if flags&flagHasSats != 0 {
		buf = append(buf, byte(p.Sats))
	}

	if !p.Timestamp.IsZero() {
		buf = binary.BigEndian.AppendUint32(buf, uint32(p.Timestamp.Unix()))
	}

	return buf
}
