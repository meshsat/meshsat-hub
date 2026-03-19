package fragment

import (
	"bytes"
	"testing"
	"time"
)

func TestHeaderRoundtrip(t *testing.T) {
	tests := []struct {
		fragIndex, fragTotal, msgID uint8
	}{
		{0, 1, 0},
		{0, 2, 5},
		{1, 2, 5},
		{3, 4, 15},
		{0, 4, 0},
		{2, 3, 7},
		{15, 16, 255},
		{0, 1, 128},
	}
	for _, tt := range tests {
		hdr := EncodeHeader(tt.fragIndex, tt.fragTotal, tt.msgID)
		gotIdx, gotTotal, gotID := DecodeHeader(hdr[0], hdr[1])
		if gotIdx != tt.fragIndex || gotTotal != tt.fragTotal || gotID != tt.msgID {
			t.Errorf("EncodeHeader(%d,%d,%d)=0x%02x%02x → DecodeHeader=(%d,%d,%d)",
				tt.fragIndex, tt.fragTotal, tt.msgID, hdr[0], hdr[1], gotIdx, gotTotal, gotID)
		}
	}
}

func TestIsFragment(t *testing.T) {
	// Helper: build a fragment-like payload of sufficient size.
	makeFrag := func(idx, total, msgID uint8) []byte {
		hdr := EncodeHeader(idx, total, msgID)
		payload := make([]byte, HeaderSize+MinFragmentPayload)
		payload[0] = hdr[0]
		payload[1] = hdr[1]
		return payload
	}

	// Single message (fragTotal=1) — not a fragment.
	if IsFragment(makeFrag(0, 1, 0)) {
		t.Error("fragTotal=1 should not be a fragment")
	}

	// Multi-fragment (fragTotal=2) — is a fragment.
	if !IsFragment(makeFrag(0, 2, 5)) {
		t.Error("fragTotal=2 should be a fragment")
	}

	// fragIndex >= fragTotal — invalid, not a fragment.
	if IsFragment(makeFrag(3, 2, 5)) {
		t.Error("fragIndex >= fragTotal should not be a fragment")
	}

	// Too short for header.
	if IsFragment([]byte{0x01}) {
		t.Error("single byte should not be a fragment")
	}

	// Has header but payload too small — not a fragment (likely non-fragmented data).
	hdr := EncodeHeader(0, 2, 5)
	if IsFragment([]byte{hdr[0], hdr[1], 0x01}) {
		t.Error("tiny payload should not be a fragment")
	}
}

func TestFragment_NoFragNeeded(t *testing.T) {
	data := make([]byte, 100)
	frags := Fragment(data, IridiumMO_MTU, 0)
	if frags != nil {
		t.Errorf("expected nil for data fitting in MTU, got %d fragments", len(frags))
	}
}

func TestFragment_ExactMTU(t *testing.T) {
	data := make([]byte, IridiumMO_MTU)
	frags := Fragment(data, IridiumMO_MTU, 0)
	if frags != nil {
		t.Errorf("expected nil for data == MTU, got %d fragments", len(frags))
	}
}

func TestFragment_TwoFragments(t *testing.T) {
	data := make([]byte, 500)
	for i := range data {
		data[i] = byte(i % 256)
	}

	frags := Fragment(data, IridiumMO_MTU, 42)
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments for 500 bytes at MTU 340, got %d", len(frags))
	}

	fragPayload := IridiumMO_MTU - HeaderSize

	// Verify headers.
	idx, total, msgID := DecodeHeader(frags[0][0], frags[0][1])
	if idx != 0 || total != 2 || msgID != 42 {
		t.Errorf("frag 0 header: idx=%d total=%d msgID=%d", idx, total, msgID)
	}
	idx, total, msgID = DecodeHeader(frags[1][0], frags[1][1])
	if idx != 1 || total != 2 || msgID != 42 {
		t.Errorf("frag 1 header: idx=%d total=%d msgID=%d", idx, total, msgID)
	}

	// Verify sizes.
	if len(frags[0]) != fragPayload+HeaderSize {
		t.Errorf("frag 0 size: got %d, want %d", len(frags[0]), fragPayload+HeaderSize)
	}
	remainderPayload := 500 - fragPayload
	if len(frags[1]) != remainderPayload+HeaderSize {
		t.Errorf("frag 1 size: got %d, want %d", len(frags[1]), remainderPayload+HeaderSize)
	}

	// Each fragment <= MTU.
	for i, f := range frags {
		if len(f) > IridiumMO_MTU {
			t.Errorf("frag %d exceeds MTU: %d > %d", i, len(f), IridiumMO_MTU)
		}
	}
}

func TestFragment_MTFragments(t *testing.T) {
	data := make([]byte, 500)
	frags := Fragment(data, IridiumMT_MTU, 1)
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments for 500 bytes at MT MTU 270, got %d", len(frags))
	}
	for i, f := range frags {
		if len(f) > IridiumMT_MTU {
			t.Errorf("frag %d exceeds MT MTU: %d > %d", i, len(f), IridiumMT_MTU)
		}
	}
}

func TestFragment_MaxFragTruncation(t *testing.T) {
	// Very large message — truncated to MaxFragments.
	mtu := 100
	fragPayload := mtu - HeaderSize
	maxData := MaxFragments * fragPayload
	data := make([]byte, maxData+500) // exceeds max
	frags := Fragment(data, mtu, 0)
	if len(frags) != MaxFragments {
		t.Fatalf("expected %d fragments (max), got %d", MaxFragments, len(frags))
	}

	// Verify total payload is truncated.
	var totalPayload int
	for _, f := range frags {
		totalPayload += len(f) - HeaderSize
	}
	if totalPayload != maxData {
		t.Errorf("total payload = %d, want %d", totalPayload, maxData)
	}
}

func TestReassembly_TwoFragments(t *testing.T) {
	data := make([]byte, 500)
	for i := range data {
		data[i] = byte(i % 256)
	}

	frags := Fragment(data, IridiumMO_MTU, 1)
	if frags == nil {
		t.Fatal("expected fragments")
	}

	r := NewReassembler(5 * time.Minute)

	// Add first fragment — should not complete.
	result, err := r.AddFragment("dev1", frags[0])
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Error("expected nil result after first fragment")
	}

	// Add second fragment — should complete.
	result, err = r.AddFragment("dev1", frags[1])
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected complete message after second fragment")
	}
	if !bytes.Equal(result, data) {
		t.Errorf("reassembled data mismatch: got %d bytes, want %d", len(result), len(data))
	}
}

func TestReassembly_OutOfOrder(t *testing.T) {
	data := make([]byte, 500)
	for i := range data {
		data[i] = byte(i % 256)
	}

	frags := Fragment(data, IridiumMO_MTU, 2)
	r := NewReassembler(5 * time.Minute)

	// Add second fragment first.
	result, _ := r.AddFragment("dev1", frags[1])
	if result != nil {
		t.Error("expected nil after out-of-order fragment")
	}

	// Add first fragment — should complete.
	result, _ = r.AddFragment("dev1", frags[0])
	if result == nil {
		t.Fatal("expected complete message after all fragments")
	}
	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

func TestReassembly_DuplicateFragment(t *testing.T) {
	data := make([]byte, 500)
	frags := Fragment(data, IridiumMO_MTU, 3)
	r := NewReassembler(5 * time.Minute)

	// Add same fragment twice — should not double-count.
	_, _ = r.AddFragment("dev1", frags[0])
	_, _ = r.AddFragment("dev1", frags[0])
	if r.PendingCount() != 1 {
		t.Errorf("expected 1 pending, got %d", r.PendingCount())
	}

	result, _ := r.AddFragment("dev1", frags[1])
	if result == nil {
		t.Fatal("expected complete after unique fragments")
	}
}

func TestReassembly_DeviceIsolation(t *testing.T) {
	data := make([]byte, 500)
	frags := Fragment(data, IridiumMO_MTU, 0)
	r := NewReassembler(5 * time.Minute)

	// Add frag 0 to dev1, frag 1 to dev2 — should NOT reassemble.
	_, _ = r.AddFragment("dev1", frags[0])
	result, _ := r.AddFragment("dev2", frags[1])
	if result != nil {
		t.Error("fragments from different devices should not reassemble")
	}
	if r.PendingCount() != 2 {
		t.Errorf("expected 2 pending (one per device), got %d", r.PendingCount())
	}
}

func TestReassembly_Expire(t *testing.T) {
	r := NewReassembler(1 * time.Millisecond)
	data := make([]byte, 500)
	frags := Fragment(data, IridiumMO_MTU, 0)

	_, _ = r.AddFragment("dev1", frags[0])
	time.Sleep(10 * time.Millisecond)

	expired := r.Expire()
	if expired != 1 {
		t.Errorf("expected 1 expired, got %d", expired)
	}
	if r.PendingCount() != 0 {
		t.Errorf("expected 0 pending after expire, got %d", r.PendingCount())
	}
}

func TestReassembly_TooShort(t *testing.T) {
	r := NewReassembler(5 * time.Minute)
	_, err := r.AddFragment("dev1", []byte{0x01})
	if err == nil {
		t.Error("expected error for single-byte fragment")
	}
}

func TestReassembly_InvalidFragIndex(t *testing.T) {
	r := NewReassembler(5 * time.Minute)
	// Header with fragTotal=1 but fragIndex=2 (invalid).
	hdr := EncodeHeader(2, 1, 0)
	_, err := r.AddFragment("dev1", []byte{hdr[0], hdr[1], 0x01})
	if err == nil {
		t.Error("expected error for fragIndex >= fragTotal")
	}
}

func TestReassembly_MsgIDIsolation(t *testing.T) {
	r := NewReassembler(5 * time.Minute)

	// Two different msgIDs from same device should not mix.
	data1 := make([]byte, 500)
	data1[0] = 0xAA
	frags1 := Fragment(data1, IridiumMO_MTU, 10)

	data2 := make([]byte, 500)
	data2[0] = 0xBB
	frags2 := Fragment(data2, IridiumMO_MTU, 11)

	_, _ = r.AddFragment("dev1", frags1[0])
	_, _ = r.AddFragment("dev1", frags2[0])
	if r.PendingCount() != 2 {
		t.Errorf("expected 2 pending (different msgIDs), got %d", r.PendingCount())
	}

	result, _ := r.AddFragment("dev1", frags1[1])
	if result == nil {
		t.Fatal("expected complete for msgID 10")
	}
	if !bytes.Equal(result, data1) {
		t.Error("reassembled data1 mismatch")
	}

	result, _ = r.AddFragment("dev1", frags2[1])
	if result == nil {
		t.Fatal("expected complete for msgID 11")
	}
	if !bytes.Equal(result, data2) {
		t.Error("reassembled data2 mismatch")
	}
}

func TestFragment_FullMsgID_Range(t *testing.T) {
	data := make([]byte, 500)
	// Test with msgID 0 and 255 (full uint8 range).
	for _, id := range []uint8{0, 127, 255} {
		frags := Fragment(data, IridiumMO_MTU, id)
		if frags == nil {
			t.Fatalf("expected fragments for msgID=%d", id)
		}
		_, _, gotID := DecodeHeader(frags[0][0], frags[0][1])
		if gotID != id {
			t.Errorf("msgID=%d: decoded as %d", id, gotID)
		}
	}
}

// --- Astrocast 1-byte fragment tests ---

func TestAstroHeaderRoundtrip(t *testing.T) {
	tests := []struct {
		msgID, fragNum, fragTotal uint8
	}{
		{0, 0, 1},
		{5, 0, 2},
		{15, 2, 3},
		{7, 3, 4},
		{0, 0, 4},
	}
	for _, tt := range tests {
		hdr := EncodeAstroHeader(tt.msgID, tt.fragNum, tt.fragTotal)
		gotMsgID, gotFragNum, gotFragTotal := DecodeAstroHeader(hdr)
		if gotMsgID != tt.msgID || gotFragNum != tt.fragNum || gotFragTotal != tt.fragTotal {
			t.Errorf("EncodeAstroHeader(%d,%d,%d)=0x%02x → DecodeAstroHeader=(%d,%d,%d)",
				tt.msgID, tt.fragNum, tt.fragTotal, hdr, gotMsgID, gotFragNum, gotFragTotal)
		}
	}
}

func TestIsAstroFragment(t *testing.T) {
	// Single fragment (fragTotal=1) — not a fragment.
	hdr := EncodeAstroHeader(5, 0, 1)
	if IsAstroFragment(append([]byte{hdr}, make([]byte, 50)...)) {
		t.Error("fragTotal=1 should not be an astro fragment")
	}

	// Multi-fragment (fragTotal=3) — is a fragment.
	hdr = EncodeAstroHeader(5, 0, 3)
	if !IsAstroFragment(append([]byte{hdr}, make([]byte, 50)...)) {
		t.Error("fragTotal=3 should be an astro fragment")
	}

	// Too short — not a fragment.
	if IsAstroFragment([]byte{}) {
		t.Error("empty should not be an astro fragment")
	}

	// Too large for Astrocast (>160 bytes) — not a fragment.
	hdr = EncodeAstroHeader(5, 0, 2)
	if IsAstroFragment(append([]byte{hdr}, make([]byte, 200)...)) {
		t.Error(">160 bytes should not be an astro fragment")
	}
}

func TestAstroReassembly_ThreeFragments(t *testing.T) {
	// Build a 400-byte message, fragment into 3 Astrocast fragments.
	data := make([]byte, 400)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var msgID uint8 = 7
	fragTotal := uint8(3) // ceil(400/159) = 3

	// Build fragments manually: [1-byte header] + [payload chunk].
	var frags [][]byte
	for i := uint8(0); i < fragTotal; i++ {
		start := int(i) * AstroFragPayload
		end := start + AstroFragPayload
		if end > len(data) {
			end = len(data)
		}
		hdr := EncodeAstroHeader(msgID, i, fragTotal)
		frag := append([]byte{hdr}, data[start:end]...)
		frags = append(frags, frag)
	}

	if len(frags) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(frags))
	}

	r := NewReassembler(5 * time.Minute)

	// Add fragments in order.
	result, err := r.AddAstroFragment("astro-dev1", frags[0])
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Error("expected nil after first fragment")
	}

	result, err = r.AddAstroFragment("astro-dev1", frags[1])
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Error("expected nil after second fragment")
	}

	result, err = r.AddAstroFragment("astro-dev1", frags[2])
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected complete message after third fragment")
	}
	if !bytes.Equal(result, data) {
		t.Errorf("reassembled data mismatch: got %d bytes, want %d", len(result), len(data))
	}
}

func TestAstroReassembly_OutOfOrder(t *testing.T) {
	data := make([]byte, 300)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var msgID uint8 = 3
	fragTotal := uint8(2)

	var frags [][]byte
	for i := uint8(0); i < fragTotal; i++ {
		start := int(i) * AstroFragPayload
		end := start + AstroFragPayload
		if end > len(data) {
			end = len(data)
		}
		hdr := EncodeAstroHeader(msgID, i, fragTotal)
		frag := append([]byte{hdr}, data[start:end]...)
		frags = append(frags, frag)
	}

	r := NewReassembler(5 * time.Minute)

	// Add second fragment first.
	result, _ := r.AddAstroFragment("dev1", frags[1])
	if result != nil {
		t.Error("expected nil after out-of-order fragment")
	}

	// Add first fragment — should complete.
	result, _ = r.AddAstroFragment("dev1", frags[0])
	if result == nil {
		t.Fatal("expected complete message")
	}
	if !bytes.Equal(result, data) {
		t.Error("reassembled data mismatch")
	}
}

// TestThreeFragmentIntegration_BothFormats tests 3-fragment reassembly for both
// Iridium 2-byte and Astrocast 1-byte formats (MESHSAT-190 acceptance criteria).
func TestThreeFragmentIntegration_BothFormats(t *testing.T) {
	r := NewReassembler(5 * time.Minute)

	// --- Iridium 2-byte: 3-fragment message ---
	iridiumData := make([]byte, 900) // 900 bytes → 3 fragments at 338B payload each
	for i := range iridiumData {
		iridiumData[i] = byte(i % 256)
	}
	iridiumFrags := Fragment(iridiumData, IridiumMO_MTU, 10)
	if len(iridiumFrags) != 3 {
		t.Fatalf("iridium: expected 3 fragments, got %d", len(iridiumFrags))
	}

	for i, frag := range iridiumFrags {
		result, err := r.AddFragment("iridium-dev", frag)
		if err != nil {
			t.Fatalf("iridium frag %d: %v", i, err)
		}
		if i < 2 && result != nil {
			t.Errorf("iridium frag %d: expected nil", i)
		}
		if i == 2 {
			if result == nil {
				t.Fatal("iridium: expected reassembled message after 3rd fragment")
			}
			if !bytes.Equal(result, iridiumData) {
				t.Error("iridium: reassembled data mismatch")
			}
		}
	}

	// --- Astrocast 1-byte: 3-fragment message ---
	astroData := make([]byte, 400) // 400 bytes → 3 fragments at 159B payload each
	for i := range astroData {
		astroData[i] = byte((i + 50) % 256)
	}

	var astroFrags [][]byte
	fragTotal := uint8(3)
	for i := uint8(0); i < fragTotal; i++ {
		start := int(i) * AstroFragPayload
		end := start + AstroFragPayload
		if end > len(astroData) {
			end = len(astroData)
		}
		hdr := EncodeAstroHeader(5, i, fragTotal)
		frag := append([]byte{hdr}, astroData[start:end]...)
		astroFrags = append(astroFrags, frag)
	}

	if len(astroFrags) != 3 {
		t.Fatalf("astrocast: expected 3 fragments, got %d", len(astroFrags))
	}

	for i, frag := range astroFrags {
		result, err := r.AddAstroFragment("astro-dev", frag)
		if err != nil {
			t.Fatalf("astrocast frag %d: %v", i, err)
		}
		if i < 2 && result != nil {
			t.Errorf("astrocast frag %d: expected nil", i)
		}
		if i == 2 {
			if result == nil {
				t.Fatal("astrocast: expected reassembled message after 3rd fragment")
			}
			if !bytes.Equal(result, astroData) {
				t.Error("astrocast: reassembled data mismatch")
			}
		}
	}
}
