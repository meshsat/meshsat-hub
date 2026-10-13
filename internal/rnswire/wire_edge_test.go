package rnswire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// ---------------------------------------------------------------------------
// Link state machine: full lifecycle (Pending→Established→Closed)
// ---------------------------------------------------------------------------

func TestPacketFlags_AllCombinations(t *testing.T) {
	// Test all valid flag combinations
	for ht := byte(0); ht <= 1; ht++ {
		for cf := byte(0); cf <= 1; cf++ {
			for tt := byte(0); tt <= 1; tt++ {
				for dt := byte(0); dt <= 3; dt++ {
					for pt := byte(0); pt <= 3; pt++ {
						h := &Header{
							HeaderType:    ht,
							ContextFlag:   cf,
							TransportType: tt,
							DestType:      dt,
							PacketType:    pt,
						}
						flags := h.PackFlags()
						h2 := &Header{}
						h2.UnpackFlags(flags)
						if h2.HeaderType != ht || h2.ContextFlag != cf || h2.TransportType != tt || h2.DestType != dt || h2.PacketType != pt {
							t.Errorf("flags roundtrip failed for ht=%d cf=%d tt=%d dt=%d pt=%d", ht, cf, tt, dt, pt)
						}
					}
				}
			}
		}
	}
}

func TestHeader_MTUBoundary(t *testing.T) {
	// Data exactly at MTU - header size
	maxData := make([]byte, MTU-HeaderMinSize)
	rand.Read(maxData)

	h := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestType:   DestSingle,
		Data:       maxData,
	}
	raw := h.Marshal()
	if len(raw) != MTU {
		t.Errorf("expected MTU-size packet (%d), got %d", MTU, len(raw))
	}

	parsed, err := UnmarshalHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.Data, maxData) {
		t.Error("data mismatch at MTU boundary")
	}
}

func TestHeader_HopBoundary(t *testing.T) {
	h := &Header{Hops: PathfinderM - 1}
	if !h.IncrementHop() {
		t.Error("should allow increment to PathfinderM")
	}
	if h.Hops != PathfinderM {
		t.Errorf("hops should be %d, got %d", PathfinderM, h.Hops)
	}
	if h.IncrementHop() {
		t.Error("should not allow increment beyond PathfinderM")
	}
}

func TestHeader_Type2WithData(t *testing.T) {
	h := &Header{
		HeaderType:    HeaderType2,
		TransportType: TransportTransport,
		DestType:      DestSingle,
		PacketType:    PacketData,
		Hops:          5,
		Context:       ContextResource,
		Data:          []byte("payload data"),
	}
	rand.Read(h.TransportID[:])
	rand.Read(h.DestHash[:])

	raw := h.Marshal()
	parsed, err := UnmarshalHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HeaderType != HeaderType2 {
		t.Error("header type mismatch")
	}
	if parsed.TransportID != h.TransportID {
		t.Error("transport ID mismatch")
	}
	if parsed.DestHash != h.DestHash {
		t.Error("dest hash mismatch")
	}
	if parsed.Context != ContextResource {
		t.Error("context mismatch")
	}
	if !bytes.Equal(parsed.Data, h.Data) {
		t.Error("data mismatch")
	}
}

func TestHeader_EmptyData(t *testing.T) {
	h := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
	}
	raw := h.Marshal()
	// RNS Packet.unpack rejects a zero-length data field, so a peer would
	// drop this packet; parsing it as malformed keeps both sides in step.
	if _, err := UnmarshalHeader(raw); err != ErrEmptyData {
		t.Fatalf("expected ErrEmptyData, got %v", err)
	}
}

func TestPacketTypeString_AllValues(t *testing.T) {
	tests := map[byte]string{
		PacketData:        "DATA",
		PacketAnnounce:    "ANNOUNCE",
		PacketLinkRequest: "LINKREQUEST",
		PacketProof:       "PROOF",
		0x04:              "UNKNOWN(04)",
		0xFF:              "UNKNOWN(ff)",
	}
	for pt, want := range tests {
		got := PacketTypeString(pt)
		if got != want {
			t.Errorf("PacketTypeString(%02x) = %q, want %q", pt, got, want)
		}
	}
}

func TestDestTypeString_AllValues(t *testing.T) {
	tests := map[byte]string{
		DestSingle: "SINGLE",
		DestGroup:  "GROUP",
		DestPlain:  "PLAIN",
		DestLink:   "LINK",
		0x04:       "UNKNOWN(04)",
	}
	for dt, want := range tests {
		got := DestTypeString(dt)
		if got != want {
			t.Errorf("DestTypeString(%02x) = %q, want %q", dt, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Announce edge cases
// ---------------------------------------------------------------------------

func TestAnnounce_WithRatchet(t *testing.T) {
	id, _ := GenerateIdentity()
	ann, err := NewAnnounce(id, "test.ratchet", []byte("app-data"))
	if err != nil {
		t.Fatal(err)
	}

	// Manually set ratchet
	ann.ContextFlag = 1
	rand.Read(ann.Ratchet[:])

	// Re-sign with ratchet included
	// (NewAnnounce doesn't set ratchet, but MarshalPayload/MarshalPacket use ContextFlag)
	payload := ann.MarshalPayload()
	if len(payload) < AnnounceRatchetPayload {
		t.Errorf("ratchet payload should be at least %d bytes, got %d", AnnounceRatchetPayload, len(payload))
	}
}

func TestAnnounce_HopIncrementPreservesVerifiability(t *testing.T) {
	id, _ := GenerateIdentity()
	ann, err := NewAnnounce(id, "test.hop", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify before incrementing
	if err := ann.Verify(); err != nil {
		t.Fatalf("should verify before hop: %v", err)
	}

	// Increment hops multiple times
	for i := 0; i < 10; i++ {
		if !ann.IncrementHop() {
			t.Fatalf("should allow increment at hop %d", i+1)
		}
	}

	// Should still verify — hops are NOT in the signed body
	if err := ann.Verify(); err != nil {
		t.Fatalf("should still verify after hop increments: %v", err)
	}
}

func TestAnnounce_MaxPacketRoundtrip(t *testing.T) {
	id, _ := GenerateIdentity()
	// Large app data (but within reason)
	appData := make([]byte, 200)
	rand.Read(appData)

	ann, err := NewAnnounce(id, "test.large", appData)
	if err != nil {
		t.Fatal(err)
	}

	raw := ann.MarshalPacket()
	parsed, err := UnmarshalAnnouncePacket(raw)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !bytes.Equal(parsed.AppData, appData) {
		t.Error("app data mismatch after round-trip")
	}
	if err := parsed.Verify(); err != nil {
		t.Fatalf("parsed announce should verify: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Confirmation edge cases
// ---------------------------------------------------------------------------

func TestHDLC_EmptyPayload(t *testing.T) {
	frame := HDLCFrame([]byte{})
	// Empty payload between two flags
	if len(frame) != 2 {
		t.Errorf("empty frame should be 2 bytes (two flags), got %d", len(frame))
	}
}

func TestHDLC_AllSpecialBytes(t *testing.T) {
	// Payload entirely of special bytes
	data := bytes.Repeat([]byte{HDLCFlag, HDLCEsc}, 50)
	escaped := HDLCEscape(data)
	unescaped := HDLCUnescape(escaped)
	if !bytes.Equal(unescaped, data) {
		t.Error("all-special-bytes round-trip failed")
	}
}

func TestHDLC_AllByteValues(t *testing.T) {
	// Every possible byte value 0x00-0xFF
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	escaped := HDLCEscape(data)
	unescaped := HDLCUnescape(escaped)
	if !bytes.Equal(unescaped, data) {
		t.Error("all-byte-values round-trip failed")
	}
}

func TestHDLCReader_BackToBackFrames(t *testing.T) {
	reader := NewHDLCFrameReader()

	// Build 3 frames packed tightly (no gap)
	var buf []byte
	payloads := [][]byte{
		make([]byte, HeaderMinSize),
		make([]byte, HeaderMinSize+10),
		make([]byte, HeaderMinSize+20),
	}
	for i, p := range payloads {
		rand.Read(p)
		// Need valid header to pass min-size check
		p[0] = byte(i) // distinguish them
		buf = append(buf, HDLCFrame(p)...)
	}

	// Feed all at once
	frames := reader.Feed(buf)
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(frames))
	}
}

func TestHDLCReader_ByteByByte(t *testing.T) {
	reader := NewHDLCFrameReader()
	payload := make([]byte, HeaderMinSize+5)
	rand.Read(payload)
	frame := HDLCFrame(payload)

	var collected [][]byte
	for _, b := range frame {
		result := reader.Feed([]byte{b})
		collected = append(collected, result...)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 frame from byte-by-byte feed, got %d", len(collected))
	}
	if !bytes.Equal(collected[0], payload) {
		t.Error("payload mismatch from byte-by-byte feed")
	}
}

func TestHDLCReader_CorruptedFrame(t *testing.T) {
	reader := NewHDLCFrameReader()

	// Frame with too-short payload (< HeaderMinSize) should be silently dropped
	short := []byte{HDLCFlag, 0x01, 0x02, HDLCFlag}
	frames := reader.Feed(short)
	if len(frames) != 0 {
		t.Errorf("too-short frame should be dropped, got %d frames", len(frames))
	}

	// Valid frame after corruption should still work
	payload := make([]byte, HeaderMinSize)
	rand.Read(payload)
	validFrame := HDLCFrame(payload)
	frames = reader.Feed(validFrame)
	if len(frames) != 1 {
		t.Fatal("valid frame after corruption should be received")
	}
}

func TestHDLCReader_GarbageBetweenFrames(t *testing.T) {
	reader := NewHDLCFrameReader()
	payload := make([]byte, HeaderMinSize)
	rand.Read(payload)

	// Garbage + valid frame
	garbage := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	buf := append(garbage, HDLCFrame(payload)...)

	frames := reader.Feed(buf)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], payload) {
		t.Error("payload mismatch after garbage prefix")
	}
}

// ---------------------------------------------------------------------------
// Identity edge cases
// ---------------------------------------------------------------------------

func TestIdentity_DifferentAppNames_DifferentDestHash(t *testing.T) {
	id, _ := GenerateIdentity()
	hash1 := id.DestHash("app.one")
	hash2 := id.DestHash("app.two")
	if hash1 == hash2 {
		t.Error("different app names should produce different dest hashes")
	}
}

func TestIdentity_LoadSaveRoundtrip(t *testing.T) {
	original, _ := GenerateIdentity()
	enc := original.EncryptionPrivateBytes()
	sig := original.SigningPrivateBytes()

	loaded, err := LoadIdentity(enc, sig)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(loaded.PublicBytes(), original.PublicBytes()) {
		t.Error("loaded public bytes should match original")
	}
	if loaded.DestHash("test") != original.DestHash("test") {
		t.Error("loaded dest hash should match original")
	}

	// Sign with loaded, verify with original
	msg := []byte("test message")
	sig2 := loaded.Sign(msg)
	if !VerifySignature(original.SigningPublicKey(), msg, sig2) {
		t.Error("signature from loaded identity should verify with original key")
	}
}

func TestVerifySignature_EdgeCases(t *testing.T) {
	if VerifySignature(nil, []byte("data"), make([]byte, ed25519.SignatureSize)) {
		t.Error("nil pubkey should fail")
	}
	id, _ := GenerateIdentity()
	if VerifySignature(id.SigningPublicKey(), []byte("data"), nil) {
		t.Error("nil signature should fail")
	}
	if VerifySignature(id.SigningPublicKey(), []byte("data"), make([]byte, 10)) {
		t.Error("short signature should fail")
	}
}
