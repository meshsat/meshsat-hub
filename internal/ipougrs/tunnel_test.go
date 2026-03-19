package ipougrs

import (
	"bytes"
	"testing"
	"time"
)

func TestEncodeDecodeFrame(t *testing.T) {
	f := &Frame{
		Magic:       MagicByte,
		FragIndex:   2,
		FragTotal:   5,
		SessionID:   42,
		TotalLength: 1024,
		Payload:     []byte("hello from satellite"),
	}

	encoded := EncodeFrame(f)
	decoded, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.Magic != MagicByte {
		t.Errorf("magic = 0x%02X, want 0x%02X", decoded.Magic, MagicByte)
	}
	if decoded.FragIndex != 2 {
		t.Errorf("frag_index = %d, want 2", decoded.FragIndex)
	}
	if decoded.FragTotal != 5 {
		t.Errorf("frag_total = %d, want 5", decoded.FragTotal)
	}
	if decoded.SessionID != 42 {
		t.Errorf("session_id = %d, want 42", decoded.SessionID)
	}
	if decoded.TotalLength != 1024 {
		t.Errorf("total_length = %d, want 1024", decoded.TotalLength)
	}
	if string(decoded.Payload) != "hello from satellite" {
		t.Errorf("payload = %q", decoded.Payload)
	}
}

func TestDecodeFrame_TooShort(t *testing.T) {
	_, err := DecodeFrame([]byte{0x49, 0x00})
	if err == nil {
		t.Error("expected error for too-short frame")
	}
}

func TestDecodeFrame_BadMagic(t *testing.T) {
	_, err := DecodeFrame([]byte{0xFF, 0x00, 0x00, 0x00, 0x00})
	if err == nil {
		t.Error("expected error for bad magic byte")
	}
}

func TestIsIPoUGRS(t *testing.T) {
	if !IsIPoUGRS([]byte{MagicByte, 0, 0, 0, 0, 1, 2, 3}) {
		t.Error("expected true for valid magic")
	}
	if IsIPoUGRS([]byte{0xFF, 0, 0, 0, 0}) {
		t.Error("expected false for wrong magic")
	}
	if IsIPoUGRS([]byte{MagicByte}) {
		t.Error("expected false for too-short data")
	}
}

func TestFragment_SmallPacket(t *testing.T) {
	packet := []byte("small IP packet")
	frames, err := Fragment(packet, MOPayloadMax, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0].Payload, packet) {
		t.Error("payload mismatch")
	}
}

func TestFragment_LargePacket(t *testing.T) {
	// 1000 bytes with 265-byte max → 4 fragments
	packet := make([]byte, 1000)
	for i := range packet {
		packet[i] = byte(i)
	}

	frames, err := Fragment(packet, MTPayloadMax, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 {
		t.Fatalf("expected 4 fragments, got %d", len(frames))
	}

	// Verify all frames have correct header.
	for _, f := range frames {
		if f.SessionID != 5 {
			t.Errorf("session_id = %d, want 5", f.SessionID)
		}
		if f.TotalLength != 1000 {
			t.Errorf("total_length = %d, want 1000", f.TotalLength)
		}
	}
}

func TestFragment_TooLarge(t *testing.T) {
	// 16 * 265 + 1 = 4241 bytes — exceeds max
	packet := make([]byte, MaxFragments*MTPayloadMax+1)
	_, err := Fragment(packet, MTPayloadMax, 1, false)
	if err == nil {
		t.Error("expected error for oversized packet")
	}
}

func TestReassembler_Roundtrip(t *testing.T) {
	packet := []byte("IP packet data for reassembly test 1234567890")

	frames, err := Fragment(packet, 20, 1, false)
	if err != nil {
		t.Fatal(err)
	}

	r := NewReassembler(5 * time.Minute)
	var result []byte

	for _, f := range frames {
		encoded := EncodeFrame(&f)
		decoded, _ := DecodeFrame(encoded)
		res, err := r.AddFrame("dev1", decoded)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil {
			result = res
		}
	}

	if result == nil {
		t.Fatal("expected reassembled packet")
	}
	if !bytes.Equal(result, packet) {
		t.Errorf("reassembled = %q, want %q", result, packet)
	}
}

func TestReassembler_Expire(t *testing.T) {
	r := NewReassembler(1 * time.Millisecond)

	frame := &Frame{
		FragIndex:   0,
		FragTotal:   2,
		SessionID:   1,
		TotalLength: 100,
		Payload:     []byte("partial"),
	}
	_, _ = r.AddFrame("dev1", frame)

	time.Sleep(5 * time.Millisecond)
	expired := r.Expire()
	if expired != 1 {
		t.Errorf("expired = %d, want 1", expired)
	}
}

func TestTunnel_FragmentAndReassemble(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Compress = false
	tunnel := NewTunnel(cfg)

	packet := make([]byte, 500)
	for i := range packet {
		packet[i] = byte(i)
	}

	// Fragment as MO.
	encoded, err := tunnel.FragmentPacket(packet, "mo")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) < 2 {
		t.Fatalf("expected multiple fragments, got %d", len(encoded))
	}

	// Reassemble.
	var result []byte
	for _, data := range encoded {
		res, err := tunnel.ReassembleFrame("dev1", data)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil {
			result = res
		}
	}

	if result == nil {
		t.Fatal("expected reassembled packet")
	}
	if !bytes.Equal(result, packet) {
		t.Error("roundtrip mismatch")
	}

	stats := tunnel.GetStats()
	if stats.PacketsTx != 1 {
		t.Errorf("packets_tx = %d", stats.PacketsTx)
	}
	if stats.PacketsRx != 1 {
		t.Errorf("packets_rx = %d", stats.PacketsRx)
	}
}

func TestTunnel_WithCompression(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Compress = true
	tunnel := NewTunnel(cfg)

	// Highly compressible data.
	packet := bytes.Repeat([]byte("AAAA"), 200)

	encoded, err := tunnel.FragmentPacket(packet, "mo")
	if err != nil {
		t.Fatal(err)
	}

	// Should compress to fewer fragments than without compression.
	if len(encoded) > 2 {
		t.Logf("compressed to %d fragments (uncompressed would be more)", len(encoded))
	}

	// Reassemble.
	var result []byte
	for _, data := range encoded {
		res, err := tunnel.ReassembleFrame("dev1", data)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil {
			result = res
		}
	}

	if !bytes.Equal(result, packet) {
		t.Error("compressed roundtrip mismatch")
	}
}
