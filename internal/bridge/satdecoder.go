// Package bridge — satellite uplink decoder for bridge-originated binary messages.
//
// When a bridge loses internet, it sends critical data (position, SOS, health)
// via Iridium satellite as compact binary frames. This decoder processes those
// frames when they arrive at the Hub via the RockBLOCK webhook.
//
// Wire format mirrors the bridge encoder (meshsat/internal/hubreporter/satuplink.go).
// NOT a shared module — binary constants and decode logic are duplicated by design.
package bridge

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// satUplinkMagic identifies bridge-originated satellite uplink messages.
var satUplinkMagic = [2]byte{0x4D, 0x53} // "MS"

// Satellite uplink message types.
const (
	SatMsgPosition      byte = 0x01
	SatMsgSOS           byte = 0x02
	SatMsgHealthSummary byte = 0x03

	satUplinkVersion byte = 1
)

const (
	satHeaderLen = 4 // magic(2) + version(1) + type(1)
)

// SatIfaceStatus represents the status of a single interface in a health summary.
type SatIfaceStatus struct {
	Name   string
	Online bool
	Signal byte // 0-5
}

// Errors returned by the decoder.
var (
	ErrSatTooShort   = errors.New("satdecoder: data too short")
	ErrSatBadMagic   = errors.New("satdecoder: invalid magic bytes")
	ErrSatBadVersion = errors.New("satdecoder: unsupported version")
	ErrSatTruncated  = errors.New("satdecoder: truncated payload")
)

// IsBridgeSatUplink checks if data starts with the satellite uplink magic bytes (0x4D 0x53).
func IsBridgeSatUplink(data []byte) bool {
	return len(data) >= satHeaderLen && data[0] == satUplinkMagic[0] && data[1] == satUplinkMagic[1]
}

// DecodeSatUplink decodes the header and returns the message type and payload (after header).
func DecodeSatUplink(data []byte) (msgType byte, payload []byte, err error) {
	if len(data) < satHeaderLen {
		return 0, nil, ErrSatTooShort
	}
	if data[0] != satUplinkMagic[0] || data[1] != satUplinkMagic[1] {
		return 0, nil, ErrSatBadMagic
	}
	if data[2] != satUplinkVersion {
		return 0, nil, ErrSatBadVersion
	}
	return data[3], data[satHeaderLen:], nil
}

func readLenPrefixedString(data []byte) (string, int, error) {
	if len(data) < 1 {
		return "", 0, ErrSatTruncated
	}
	n := int(data[0])
	if len(data) < 1+n {
		return "", 0, ErrSatTruncated
	}
	return string(data[1 : 1+n]), 1 + n, nil
}

// DecodeSatPosition decodes a position payload (after header).
func DecodeSatPosition(payload []byte) (bridgeID string, lat, lon float64, alt float32, source byte, timestamp time.Time, err error) {
	off := 0
	bridgeID, n, err := readLenPrefixedString(payload[off:])
	if err != nil {
		return
	}
	off += n
	if off+4+4+2+1+4 > len(payload) {
		err = ErrSatTruncated
		return
	}
	lat = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[off:])))
	off += 4
	lon = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[off:])))
	off += 4
	alt = float32(int16(binary.BigEndian.Uint16(payload[off:])))
	off += 2
	source = payload[off]
	off++
	timestamp = time.Unix(int64(binary.BigEndian.Uint32(payload[off:])), 0).UTC()
	return
}

// DecodeSatSOS decodes an SOS payload (after header).
func DecodeSatSOS(payload []byte) (bridgeID, deviceID string, lat, lon float64, message string, timestamp time.Time, err error) {
	off := 0
	bridgeID, n, err := readLenPrefixedString(payload[off:])
	if err != nil {
		return
	}
	off += n
	deviceID, n, err = readLenPrefixedString(payload[off:])
	if err != nil {
		return
	}
	off += n
	if off+4+4 > len(payload) {
		err = ErrSatTruncated
		return
	}
	lat = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[off:])))
	off += 4
	lon = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[off:])))
	off += 4
	message, n, err = readLenPrefixedString(payload[off:])
	if err != nil {
		return
	}
	off += n
	if off+4 > len(payload) {
		err = ErrSatTruncated
		return
	}
	timestamp = time.Unix(int64(binary.BigEndian.Uint32(payload[off:])), 0).UTC()
	return
}

// DecodeSatHealth decodes a health summary payload (after header).
func DecodeSatHealth(payload []byte) (bridgeID string, uptimeSec uint32, cpuPct, memPct, diskPct byte, interfaces []SatIfaceStatus, timestamp time.Time, err error) {
	off := 0
	bridgeID, n, err := readLenPrefixedString(payload[off:])
	if err != nil {
		return
	}
	off += n
	if off+4+1+1+1+1 > len(payload) {
		err = ErrSatTruncated
		return
	}
	uptimeSec = binary.BigEndian.Uint32(payload[off:])
	off += 4
	cpuPct = payload[off]
	off++
	memPct = payload[off]
	off++
	diskPct = payload[off]
	off++
	ifaceCount := int(payload[off])
	off++
	interfaces = make([]SatIfaceStatus, 0, ifaceCount)
	for i := 0; i < ifaceCount; i++ {
		var name string
		name, n, err = readLenPrefixedString(payload[off:])
		if err != nil {
			return
		}
		off += n
		if off+2 > len(payload) {
			err = ErrSatTruncated
			return
		}
		iface := SatIfaceStatus{
			Name:   name,
			Online: payload[off] != 0,
			Signal: payload[off+1],
		}
		off += 2
		interfaces = append(interfaces, iface)
	}
	if off+4 > len(payload) {
		err = ErrSatTruncated
		return
	}
	timestamp = time.Unix(int64(binary.BigEndian.Uint32(payload[off:])), 0).UTC()
	return
}
