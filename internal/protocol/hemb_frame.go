package protocol

// HeMB frame format — ported from meshsat/internal/hemb/frame.go.
// Hub only receives extended headers (compact headers are promoted to extended
// by bridges before MQTT relay). Marshal is included for test round-trips and
// future Hub→bridge control frames.

import (
	"errors"
	"fmt"
)

// Frame format constants.
const (
	HeMBCompactHeaderLen  = 8
	HeMBExtendedHeaderLen = 16
	HeMBMagicByte0        = 0x48 // 'H'
	HeMBMagicByte1        = 0x4D // 'M'
	HeMBVersionV1         = 0x00 // 2-bit version field
)

// Frame flags (2 bits).
const (
	HeMBFlagData   = 0x00
	HeMBFlagRepair = 0x01
	HeMBFlagAck    = 0x02
	HeMBFlagCtrl   = 0x03
)

// Extended flags (byte 14, individual bits).
const (
	HeMBFlagExtHasFECMeta = 1 << iota // bit0: FEC metadata present
	HeMBFlagExtSystematic             // bit1: systematic coding
	HeMBFlagExtPriority               // bit2: high-priority frame
)

var (
	ErrHeMBFrameTooShort = errors.New("hemb: frame too short")
	ErrHeMBBadCRC        = errors.New("hemb: CRC-8 mismatch")
	ErrHeMBBadMagic      = errors.New("hemb: bad magic bytes")
)

// HeMBExtendedHeader is the 16-byte extended frame header.
type HeMBExtendedHeader struct {
	Version          uint8
	StreamID         uint8  // 8 bits (0-255)
	Flags            uint8  // 2 bits (data/repair/ack/ctrl)
	Sequence         uint16 // 16-bit symbol index
	K                uint8  // source symbols in generation (1-255)
	N                uint8  // total coded symbols (1-255)
	BearerIndex      uint8  // 8-bit bearer identifier
	GenerationID     uint16 // 16-bit generation within stream
	TotalPayloadSize uint16 // original payload size before splitting
	TTL              uint8  // units of 10 seconds (0-255, max 42.5 min)
	FlagsExtended    uint8  // bit0=has_fec_meta, bit1=systematic, bit2=priority
}

// hembCRC8 computes CRC-8 (ITU-T polynomial 0x07).
func hembCRC8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// MarshalHeMBExtended encodes an HeMBExtendedHeader into 16 bytes.
// Wire-compatible with meshsat/internal/hemb/frame.go MarshalExtended.
func MarshalHeMBExtended(h HeMBExtendedHeader) [HeMBExtendedHeaderLen]byte {
	var b [HeMBExtendedHeaderLen]byte
	b[0] = HeMBMagicByte0
	b[1] = HeMBMagicByte1
	b[2] = (h.Version&0x03)<<6 | (h.StreamID&0x0F)<<2 | (h.Flags & 0x03)
	b[3] = h.StreamID
	b[4] = byte(h.Sequence)
	b[5] = byte(h.Sequence >> 8)
	b[6] = h.K
	b[7] = h.N
	b[8] = h.BearerIndex
	b[9] = byte(h.GenerationID)
	b[10] = byte(h.GenerationID >> 8)
	b[11] = byte(h.TotalPayloadSize)
	b[12] = byte(h.TotalPayloadSize >> 8)
	b[13] = h.TTL
	b[14] = h.FlagsExtended
	b[15] = hembCRC8(b[:15])
	return b
}

// UnmarshalHeMBExtended decodes 16 bytes into an HeMBExtendedHeader.
func UnmarshalHeMBExtended(b [HeMBExtendedHeaderLen]byte) (HeMBExtendedHeader, error) {
	if b[0] != HeMBMagicByte0 || b[1] != HeMBMagicByte1 {
		return HeMBExtendedHeader{}, ErrHeMBBadMagic
	}
	if hembCRC8(b[:15]) != b[15] {
		return HeMBExtendedHeader{}, ErrHeMBBadCRC
	}
	return HeMBExtendedHeader{
		Version:          (b[2] >> 6) & 0x03,
		StreamID:         b[3],
		Flags:            b[2] & 0x03,
		Sequence:         uint16(b[4]) | uint16(b[5])<<8,
		K:                b[6],
		N:                b[7],
		BearerIndex:      b[8],
		GenerationID:     uint16(b[9]) | uint16(b[10])<<8,
		TotalPayloadSize: uint16(b[11]) | uint16(b[12])<<8,
		TTL:              b[13],
		FlagsExtended:    b[14],
	}, nil
}

// IsHeMBFrame returns true if data starts with a valid HeMB frame header.
func IsHeMBFrame(data []byte) bool {
	if len(data) >= HeMBExtendedHeaderLen && data[0] == HeMBMagicByte0 && data[1] == HeMBMagicByte1 {
		var b [HeMBExtendedHeaderLen]byte
		copy(b[:], data[:HeMBExtendedHeaderLen])
		_, err := UnmarshalHeMBExtended(b)
		return err == nil
	}
	// Also detect compact headers via CRC-8 validation.
	if len(data) >= HeMBCompactHeaderLen {
		return hembCRC8(data[:HeMBCompactHeaderLen-1]) == data[HeMBCompactHeaderLen-1]
	}
	return false
}

// ParseHeMBSymbol extracts a CodedSymbol from a framed HeMB message.
// Supports both extended and compact frame formats.
func ParseHeMBSymbol(data []byte) (sym HeMBCodedSymbol, streamID uint8, bearerIdx uint8, err error) {
	if len(data) >= HeMBExtendedHeaderLen && data[0] == HeMBMagicByte0 && data[1] == HeMBMagicByte1 {
		var b [HeMBExtendedHeaderLen]byte
		copy(b[:], data[:HeMBExtendedHeaderLen])
		hdr, e := UnmarshalHeMBExtended(b)
		if e != nil {
			return HeMBCodedSymbol{}, 0, 0, e
		}
		k := int(hdr.K)
		coeffEnd := HeMBExtendedHeaderLen + k
		if len(data) < coeffEnd+1 {
			return HeMBCodedSymbol{}, 0, 0, fmt.Errorf("hemb: frame too short for K=%d", k)
		}
		sym = HeMBCodedSymbol{
			GenID:        hdr.GenerationID,
			SymbolIndex:  int(hdr.Sequence),
			K:            k,
			Coefficients: make([]byte, k),
			Data:         make([]byte, len(data)-coeffEnd),
		}
		copy(sym.Coefficients, data[HeMBExtendedHeaderLen:coeffEnd])
		copy(sym.Data, data[coeffEnd:])
		return sym, hdr.StreamID, hdr.BearerIndex, nil
	}

	// Compact header fallback.
	if len(data) >= HeMBCompactHeaderLen {
		if hembCRC8(data[:HeMBCompactHeaderLen-1]) != data[HeMBCompactHeaderLen-1] {
			return HeMBCodedSymbol{}, 0, 0, ErrHeMBBadCRC
		}
		b0 := data[0]
		streamID = (b0 >> 2) & 0x0F
		k := int(data[2])
		bearerIdx = (data[4] >> 4) & 0x0F
		genID := uint16(data[5]) | uint16(data[6]&0xC0)<<2
		seqLo := data[1]
		seqHi := data[4] & 0x0F
		seq := int(seqLo) | int(seqHi)<<8

		coeffEnd := HeMBCompactHeaderLen + k
		if len(data) < coeffEnd+1 {
			return HeMBCodedSymbol{}, 0, 0, fmt.Errorf("hemb: compact frame too short for K=%d", k)
		}
		sym = HeMBCodedSymbol{
			GenID:        genID,
			SymbolIndex:  seq,
			K:            k,
			Coefficients: make([]byte, k),
			Data:         make([]byte, len(data)-coeffEnd),
		}
		copy(sym.Coefficients, data[HeMBCompactHeaderLen:coeffEnd])
		copy(sym.Data, data[coeffEnd:])
		return sym, streamID, bearerIdx, nil
	}

	return HeMBCodedSymbol{}, 0, 0, ErrHeMBFrameTooShort
}
