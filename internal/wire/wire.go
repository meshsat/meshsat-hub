// Package wire reinterprets fixed-width two's-complement fields of binary
// wire formats. Each helper is the one place where a signed value is read
// from or written to its unsigned encoding, so the intentional
// reinterpretation is reviewed once instead of at every codec site.
package wire

import "encoding/binary"

// I32BE reads a big-endian signed 32-bit field.
func I32BE(b []byte) int32 { return int32(binary.BigEndian.Uint32(b)) } // #nosec G115 -- two's-complement wire field

// I32LE reads a little-endian signed 32-bit field.
func I32LE(b []byte) int32 { return int32(binary.LittleEndian.Uint32(b)) } // #nosec G115 -- two's-complement wire field

// I16BE reads a big-endian signed 16-bit field.
func I16BE(b []byte) int16 { return int16(binary.BigEndian.Uint16(b)) } // #nosec G115 -- two's-complement wire field

// I16LE reads a little-endian signed 16-bit field.
func I16LE(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b)) } // #nosec G115 -- two's-complement wire field

// I8 reads a signed 8-bit field.
func I8(b byte) int8 { return int8(b) } // #nosec G115 -- two's-complement wire field

// U32 returns the two's-complement encoding of v for a 32-bit field.
func U32(v int32) uint32 { return uint32(v) } // #nosec G115 -- two's-complement wire field

// U16 returns the two's-complement encoding of v for a 16-bit field.
func U16(v int16) uint16 { return uint16(v) } // #nosec G115 -- two's-complement wire field

// ClampU8 saturates v into 0..255.
func ClampU8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// ClampU16 saturates v into 0..65535.
func ClampU16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}
