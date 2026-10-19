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
	"github.com/meshsat/meshsat-hub/internal/wire"
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
	// SatMsgSOSStart and SatMsgSOSCancel carry an SOS's identity (HeMB-NG C10, r4;
	// docs/fixtures/v1/sos.json): 0x07 is the 0x02 body, then sos_id (16) and
	// run_start_ms (8, BE); 0x08 is bridge id (1-byte length + bytes), sos_id (16),
	// run_start_ms (8, BE), unix seconds (4, BE) and a reason (1).
	SatMsgSOSStart  byte = 0x07
	SatMsgSOSCancel byte = 0x08

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
	// The version byte is part of the test, not just the magic. An out-of-band
	// management frame travels as TEXT beginning "MS:", whose first two bytes
	// are this same magic (0x4D 0x53) followed by ':' (0x3A). Judged on two
	// bytes, every OOB reply over satellite was taken for an uplink frame,
	// failed the version check one step later and logged a decode warning
	// before falling through to the classifier it belonged to (seen on the
	// first PING over IMT, 2026-09-21). A future uplink version has to be
	// added here deliberately, which is the right amount of friction.
	return len(data) >= satHeaderLen && data[0] == satUplinkMagic[0] && data[1] == satUplinkMagic[1] && data[2] == satUplinkVersion
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
	alt = float32(wire.I16BE(payload[off:]))
	off += 2
	source = payload[off]
	off++
	timestamp = time.Unix(int64(binary.BigEndian.Uint32(payload[off:])), 0).UTC()
	return
}

// DecodeSatSOS decodes an SOS payload (after header).
func DecodeSatSOS(payload []byte) (bridgeID, deviceID string, lat, lon float64, message string, timestamp time.Time, err error) {
	bridgeID, deviceID, lat, lon, message, timestamp, _, err = decodeSOSBody(payload)
	return
}

// decodeSOSBody decodes the 0x02 body and says how many bytes it took.
func decodeSOSBody(payload []byte) (bridgeID, deviceID string, lat, lon float64, message string, timestamp time.Time, used int, err error) {
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
	used = off + 4
	return
}

// SOSStartFrame is a decoded 0x07: an SOS with its identity.
type SOSStartFrame struct {
	BridgeID, DeviceID, Message string
	Lat, Lon                    float64
	Timestamp                   time.Time
	SOSID                       [16]byte
	RunStartMs                  int64
}

// SOSCancelFrame is a decoded 0x08: the cancel of the SOS SOSID.
type SOSCancelFrame struct {
	BridgeID   string
	SOSID      [16]byte
	RunStartMs int64
	Timestamp  time.Time
	Reason     byte
}

// ErrSatTrailing is returned for a C10 frame longer than its layout.
var ErrSatTrailing = errors.New("satdecoder: trailing bytes")

// readRunStart reads the 8-byte big-endian run_start_ms, refusing one beyond int64.
func readRunStart(b []byte) (int64, error) {
	v := binary.BigEndian.Uint64(b)
	if v > math.MaxInt64 {
		return 0, ErrSatTruncated
	}
	return int64(v), nil // #nosec G115 -- checked above
}

// DecodeSatSOSStart decodes a 0x07 payload (after header); its length must be exact.
func DecodeSatSOSStart(payload []byte) (f SOSStartFrame, err error) {
	var used int
	f.BridgeID, f.DeviceID, f.Lat, f.Lon, f.Message, f.Timestamp, used, err = decodeSOSBody(payload)
	if err != nil {
		return f, err
	}
	rest := payload[used:]
	if len(rest) < 24 {
		return f, ErrSatTruncated
	}
	if len(rest) > 24 {
		return f, ErrSatTrailing
	}
	copy(f.SOSID[:], rest[:16])
	f.RunStartMs, err = readRunStart(rest[16:24])
	return f, err
}

// DecodeSatSOSCancel decodes a 0x08 payload (after header); its length must be exact.
func DecodeSatSOSCancel(payload []byte) (f SOSCancelFrame, err error) {
	id, n, err := readLenPrefixedString(payload)
	if err != nil {
		return f, err
	}
	f.BridgeID = id
	rest := payload[n:]
	if len(rest) < 16+8+4+1 {
		return f, ErrSatTruncated
	}
	if len(rest) > 16+8+4+1 {
		return f, ErrSatTrailing
	}
	copy(f.SOSID[:], rest[:16])
	if f.RunStartMs, err = readRunStart(rest[16:24]); err != nil {
		return f, err
	}
	f.Timestamp = time.Unix(int64(binary.BigEndian.Uint32(rest[24:28])), 0).UTC()
	f.Reason = rest[28]
	return f, nil
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
