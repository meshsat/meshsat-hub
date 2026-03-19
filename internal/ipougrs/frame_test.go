package ipougrs

import (
	"bytes"
	"testing"
	"time"
)

func TestFrameEncodeDecodeRoundTrip(t *testing.T) {
	original := &Frame{
		FragIndex: 2,
		FragTotal: 5,
		PacketID:  42,
		Flags:     FlagCompressed,
		Payload:   []byte("hello world"),
	}

	encoded := original.Encode()
	if encoded[0] != Magic {
		t.Fatalf("magic byte: got 0x%02x, want 0x%02x", encoded[0], Magic)
	}

	decoded, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}

	if decoded.FragIndex != original.FragIndex {
		t.Errorf("FragIndex: got %d, want %d", decoded.FragIndex, original.FragIndex)
	}
	if decoded.FragTotal != original.FragTotal {
		t.Errorf("FragTotal: got %d, want %d", decoded.FragTotal, original.FragTotal)
	}
	if decoded.PacketID != original.PacketID {
		t.Errorf("PacketID: got %d, want %d", decoded.PacketID, original.PacketID)
	}
	if decoded.Flags != original.Flags {
		t.Errorf("Flags: got 0x%02x, want 0x%02x", decoded.Flags, original.Flags)
	}
	if !bytes.Equal(decoded.Payload, original.Payload) {
		t.Errorf("Payload: got %q, want %q", decoded.Payload, original.Payload)
	}
}

func TestDecodeFrameTooShort(t *testing.T) {
	_, err := DecodeFrame([]byte{0x49, 0x00})
	if err == nil {
		t.Fatal("expected error for short frame")
	}
}

func TestDecodeFrameBadMagic(t *testing.T) {
	_, err := DecodeFrame([]byte{0xFF, 0x00, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestDecodeFrameInvalidFragIndex(t *testing.T) {
	// frag_index=5, frag_total=3 (encoded as 2) → 5 >= 3, invalid
	_, err := DecodeFrame([]byte{Magic, 0x52, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for frag_index >= frag_total")
	}
}

func TestIsIPoUGRS(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"valid", []byte{Magic, 0x00, 0x00, 0x00, 0x01}, true},
		{"too short", []byte{Magic, 0x00}, false},
		{"wrong magic", []byte{0xFF, 0x00, 0x00, 0x00}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIPoUGRS(tt.data); got != tt.want {
				t.Errorf("IsIPoUGRS: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFragmentPacketNoFragmentation(t *testing.T) {
	packet := bytes.Repeat([]byte{0x45}, 100) // small packet
	frames, err := FragmentPacket(packet, 270, 1, false)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].FragIndex != 0 || frames[0].FragTotal != 1 {
		t.Errorf("single frame should have index=0, total=1")
	}
}

func TestFragmentPacketMultipleFragments(t *testing.T) {
	// Create a packet that exceeds SBD MTU.
	packet := bytes.Repeat([]byte{0x45}, 500)
	frames, err := FragmentPacket(packet, 270, 7, false)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("expected multiple frames, got %d", len(frames))
	}

	// Verify all frames have consistent metadata.
	for i, f := range frames {
		if f.FragIndex != uint8(i) {
			t.Errorf("frame %d: FragIndex=%d, want %d", i, f.FragIndex, i)
		}
		if f.FragTotal != uint8(len(frames)) {
			t.Errorf("frame %d: FragTotal=%d, want %d", i, f.FragTotal, len(frames))
		}
		if f.PacketID != 7 {
			t.Errorf("frame %d: PacketID=%d, want 7", i, f.PacketID)
		}
	}

	// Verify total payload size matches original.
	var totalPayload int
	for _, f := range frames {
		totalPayload += len(f.Payload)
	}
	if totalPayload != len(packet) {
		t.Errorf("total payload %d != original packet %d", totalPayload, len(packet))
	}
}

func TestFragmentPacketWithCompression(t *testing.T) {
	// Highly compressible data.
	packet := bytes.Repeat([]byte{0x00}, 500)
	frames, err := FragmentPacket(packet, 270, 1, true)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}

	// Compressed version should fit in fewer frames (or 1).
	if len(frames) > 1 {
		t.Logf("compressed into %d frames (uncompressed would be 2+)", len(frames))
	}
	if frames[0].Flags&FlagCompressed == 0 {
		t.Error("expected FlagCompressed to be set")
	}
}

func TestFragmentPacketTooLarge(t *testing.T) {
	// Packet that would need >16 fragments (exceeds MaxFragments).
	packet := bytes.Repeat([]byte{0x45}, 5000)
	_, err := FragmentPacket(packet, 270, 1, false)
	if err == nil {
		t.Fatal("expected error for oversized packet")
	}
}

func TestFragmentPacketEmpty(t *testing.T) {
	frames, err := FragmentPacket(nil, 270, 1, false)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}
	if frames != nil {
		t.Fatalf("expected nil for empty packet, got %d frames", len(frames))
	}
}

func TestReassemblerSingleFrame(t *testing.T) {
	reasm := NewPacketReassembler(time.Minute)

	frame := &Frame{
		FragIndex: 0,
		FragTotal: 1,
		PacketID:  1,
		Payload:   []byte("complete packet"),
	}

	packet, err := reasm.AddFrame("dev1", frame)
	if err != nil {
		t.Fatalf("AddFrame: %v", err)
	}
	if !bytes.Equal(packet, []byte("complete packet")) {
		t.Errorf("got %q, want %q", packet, "complete packet")
	}
}

func TestReassemblerMultipleFrames(t *testing.T) {
	reasm := NewPacketReassembler(time.Minute)
	original := []byte("ABCDEFGHIJ")

	// Simulate 3-fragment split.
	frames := []*Frame{
		{FragIndex: 0, FragTotal: 3, PacketID: 5, Payload: []byte("ABCD")},
		{FragIndex: 1, FragTotal: 3, PacketID: 5, Payload: []byte("EFG")},
		{FragIndex: 2, FragTotal: 3, PacketID: 5, Payload: []byte("HIJ")},
	}

	// First two frames should return nil.
	for i := 0; i < 2; i++ {
		packet, err := reasm.AddFrame("dev1", frames[i])
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if packet != nil {
			t.Fatalf("frame %d: expected nil, got packet", i)
		}
	}

	// Third frame completes reassembly.
	packet, err := reasm.AddFrame("dev1", frames[2])
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if !bytes.Equal(packet, original) {
		t.Errorf("reassembled: got %q, want %q", packet, original)
	}
}

func TestReassemblerOutOfOrder(t *testing.T) {
	reasm := NewPacketReassembler(time.Minute)

	frames := []*Frame{
		{FragIndex: 2, FragTotal: 3, PacketID: 1, Payload: []byte("C")},
		{FragIndex: 0, FragTotal: 3, PacketID: 1, Payload: []byte("A")},
		{FragIndex: 1, FragTotal: 3, PacketID: 1, Payload: []byte("B")},
	}

	var packet []byte
	var err error
	for _, f := range frames {
		packet, err = reasm.AddFrame("dev1", f)
		if err != nil {
			t.Fatalf("AddFrame: %v", err)
		}
	}

	if !bytes.Equal(packet, []byte("ABC")) {
		t.Errorf("got %q, want %q", packet, "ABC")
	}
}

func TestReassemblerDeviceIsolation(t *testing.T) {
	reasm := NewPacketReassembler(time.Minute)

	// Same packetID but different devices should not interfere.
	f1 := &Frame{FragIndex: 0, FragTotal: 2, PacketID: 1, Payload: []byte("A")}
	f2 := &Frame{FragIndex: 0, FragTotal: 2, PacketID: 1, Payload: []byte("X")}

	packet, _ := reasm.AddFrame("dev1", f1)
	if packet != nil {
		t.Fatal("expected nil for incomplete reassembly")
	}
	packet, _ = reasm.AddFrame("dev2", f2)
	if packet != nil {
		t.Fatal("expected nil for incomplete reassembly")
	}

	if reasm.PendingCount() != 2 {
		t.Errorf("pending count: got %d, want 2", reasm.PendingCount())
	}
}

func TestReassemblerExpiry(t *testing.T) {
	reasm := NewPacketReassembler(1 * time.Millisecond)

	f := &Frame{FragIndex: 0, FragTotal: 2, PacketID: 1, Payload: []byte("A")}
	_, _ = reasm.AddFrame("dev1", f)

	time.Sleep(5 * time.Millisecond)
	expired := reasm.Expire()
	if expired != 1 {
		t.Errorf("expired: got %d, want 1", expired)
	}
	if reasm.PendingCount() != 0 {
		t.Errorf("pending after expiry: got %d, want 0", reasm.PendingCount())
	}
}

func TestReassemblerWithCompression(t *testing.T) {
	reasm := NewPacketReassembler(time.Minute)

	// Compress some data, then reassemble.
	original := bytes.Repeat([]byte{0x00}, 200)
	compressed, err := deflateCompress(original)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}

	f := &Frame{
		FragIndex: 0,
		FragTotal: 1,
		PacketID:  1,
		Flags:     FlagCompressed,
		Payload:   compressed,
	}

	packet, err := reasm.AddFrame("dev1", f)
	if err != nil {
		t.Fatalf("AddFrame: %v", err)
	}
	if !bytes.Equal(packet, original) {
		t.Errorf("decompressed packet length: got %d, want %d", len(packet), len(original))
	}
}

func TestFragmentAndReassembleRoundTrip(t *testing.T) {
	// Full round-trip: fragment an IP packet, encode frames, decode frames, reassemble.
	original := bytes.Repeat([]byte{0x45}, 800)
	sbdMTU := 270

	frames, err := FragmentPacket(original, sbdMTU, 42, false)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("expected multiple fragments, got %d", len(frames))
	}

	reasm := NewPacketReassembler(time.Minute)
	var reassembled []byte

	for _, frame := range frames {
		// Encode to wire format and decode back.
		wire := frame.Encode()
		decoded, err := DecodeFrame(wire)
		if err != nil {
			t.Fatalf("DecodeFrame: %v", err)
		}

		packet, err := reasm.AddFrame("test-device", decoded)
		if err != nil {
			t.Fatalf("AddFrame: %v", err)
		}
		if packet != nil {
			reassembled = packet
		}
	}

	if !bytes.Equal(reassembled, original) {
		t.Errorf("round-trip failed: got %d bytes, want %d", len(reassembled), len(original))
	}
}

func TestFragmentAndReassembleCompressedRoundTrip(t *testing.T) {
	// Round-trip with compression enabled.
	original := bytes.Repeat([]byte("IP packet data "), 50) // 750 bytes, compressible
	sbdMTU := 270

	frames, err := FragmentPacket(original, sbdMTU, 1, true)
	if err != nil {
		t.Fatalf("FragmentPacket: %v", err)
	}

	reasm := NewPacketReassembler(time.Minute)
	var reassembled []byte

	for _, frame := range frames {
		wire := frame.Encode()
		decoded, err := DecodeFrame(wire)
		if err != nil {
			t.Fatalf("DecodeFrame: %v", err)
		}
		packet, err := reasm.AddFrame("test-device", decoded)
		if err != nil {
			t.Fatalf("AddFrame: %v", err)
		}
		if packet != nil {
			reassembled = packet
		}
	}

	if !bytes.Equal(reassembled, original) {
		t.Errorf("compressed round-trip failed: got %d bytes, want %d", len(reassembled), len(original))
	}
}

func TestTunnelFragmentForSend(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Compress = false
	tunnel := NewTunnel(cfg)

	packet := bytes.Repeat([]byte{0x45}, 500)
	encoded, err := tunnel.FragmentForSend(packet, 270)
	if err != nil {
		t.Fatalf("FragmentForSend: %v", err)
	}
	if len(encoded) < 2 {
		t.Fatalf("expected multiple encoded frames, got %d", len(encoded))
	}

	stats := tunnel.GetStats()
	if stats.PacketsTx != 1 {
		t.Errorf("PacketsTx: got %d, want 1", stats.PacketsTx)
	}
	if stats.FragmentsTx != uint64(len(encoded)) {
		t.Errorf("FragmentsTx: got %d, want %d", stats.FragmentsTx, len(encoded))
	}
}

func TestTunnelReassembleFrame(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Compress = false
	tunnel := NewTunnel(cfg)

	// Create a single-fragment frame.
	frame := &Frame{
		FragIndex: 0,
		FragTotal: 1,
		PacketID:  1,
		Payload:   []byte("test packet"),
	}
	wire := frame.Encode()

	packet, err := tunnel.ReassembleFrame("dev1", wire)
	if err != nil {
		t.Fatalf("ReassembleFrame: %v", err)
	}
	if !bytes.Equal(packet, []byte("test packet")) {
		t.Errorf("got %q, want %q", packet, "test packet")
	}

	stats := tunnel.GetStats()
	if stats.PacketsRx != 1 {
		t.Errorf("PacketsRx: got %d, want 1", stats.PacketsRx)
	}
	if stats.FragmentsRx != 1 {
		t.Errorf("FragmentsRx: got %d, want 1", stats.FragmentsRx)
	}
	if stats.ReassemblyOK != 1 {
		t.Errorf("ReassemblyOK: got %d, want 1", stats.ReassemblyOK)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled {
		t.Error("tunnel should be disabled by default")
	}
	if cfg.SBDMTU != 270 {
		t.Errorf("SBDMTU: got %d, want 270", cfg.SBDMTU)
	}
	if cfg.MTU != 1400 {
		t.Errorf("MTU: got %d, want 1400", cfg.MTU)
	}
	if cfg.FragTimeout != 2*time.Minute {
		t.Errorf("FragTimeout: got %v, want 2m", cfg.FragTimeout)
	}
}
