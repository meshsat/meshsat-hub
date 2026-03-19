package codec

import (
	"bytes"
	"testing"
)

func TestStripVersionByte_V1(t *testing.T) {
	payload := []byte{ProtoVersion1, 0xDE, 0xAD}
	ver, data := StripVersionByte(payload)
	if ver != ProtoVersion1 {
		t.Errorf("version = 0x%02X, want 0x%02X", ver, ProtoVersion1)
	}
	if !bytes.Equal(data, []byte{0xDE, 0xAD}) {
		t.Errorf("data = %x, want dead", data)
	}
}

func TestStripVersionByte_KnownMagic(t *testing.T) {
	magics := []byte{MagicGPSBridgeFull, MagicGPSBridgeDelta, MagicGPSHubFormat,
		MagicCannedMessage, MagicIPoUGRS, MagicJSON}

	for _, m := range magics {
		payload := []byte{m, 0x01, 0x02}
		ver, data := StripVersionByte(payload)
		if ver != 0 {
			t.Errorf("magic 0x%02X: version = %d, want 0 (legacy)", m, ver)
		}
		if !bytes.Equal(data, payload) {
			t.Errorf("magic 0x%02X: data should be unchanged", m)
		}
	}
}

func TestStripVersionByte_Legacy(t *testing.T) {
	payload := []byte{0xFF, 0x01} // unknown first byte
	ver, data := StripVersionByte(payload)
	if ver != 0 {
		t.Errorf("version = %d, want 0 (legacy)", ver)
	}
	if !bytes.Equal(data, payload) {
		t.Error("legacy data should be unchanged")
	}
}

func TestStripVersionByte_Empty(t *testing.T) {
	ver, data := StripVersionByte(nil)
	if ver != 0 || len(data) != 0 {
		t.Error("empty should return version 0 and empty data")
	}
}

func TestPrependVersionByte(t *testing.T) {
	payload := []byte{0xDE, 0xAD}
	versioned := PrependVersionByte(payload)
	if versioned[0] != ProtoVersion1 {
		t.Errorf("first byte = 0x%02X, want 0x%02X", versioned[0], ProtoVersion1)
	}
	if !bytes.Equal(versioned[1:], payload) {
		t.Error("payload after version byte should match original")
	}
}

func TestStripThenPrependRoundtrip(t *testing.T) {
	original := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	versioned := PrependVersionByte(original)
	ver, data := StripVersionByte(versioned)
	if ver != ProtoVersion1 {
		t.Errorf("roundtrip version = %d", ver)
	}
	if !bytes.Equal(data, original) {
		t.Error("roundtrip data mismatch")
	}
}
