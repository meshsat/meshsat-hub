package wire

import "testing"

func TestRoundTrip(t *testing.T) {
	var b [4]byte
	b[0], b[1], b[2], b[3] = 0xFF, 0xFF, 0xFF, 0xFE
	if I32BE(b[:]) != -2 {
		t.Fatalf("I32BE = %d", I32BE(b[:]))
	}
	if U32(-2) != 0xFFFFFFFE || U16(-1) != 0xFFFF {
		t.Fatal("U32/U16 encoding")
	}
	if I16LE([]byte{0xFE, 0xFF}) != -2 || I16BE([]byte{0xFF, 0xFE}) != -2 || I32LE([]byte{0xFE, 0xFF, 0xFF, 0xFF}) != -2 {
		t.Fatal("16/32-bit little/big endian decode")
	}
	if I8(0x80) != -128 {
		t.Fatal("I8")
	}
	if ClampU8(-5) != 0 || ClampU8(300) != 255 || ClampU8(7) != 7 {
		t.Fatal("ClampU8")
	}
	if ClampU16(-1) != 0 || ClampU16(70000) != 65535 || ClampU16(9) != 9 {
		t.Fatal("ClampU16")
	}
}
