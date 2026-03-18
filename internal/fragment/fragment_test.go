package fragment

import (
	"bytes"
	"testing"
	"time"
)

func TestHeaderRoundtrip(t *testing.T) {
	tests := []struct {
		msgID, fragNum, fragTotal uint8
	}{
		{0, 0, 1},
		{5, 0, 2},
		{5, 1, 2},
		{15, 3, 4},
		{0, 0, 4},
		{7, 2, 3},
	}
	for _, tt := range tests {
		b := EncodeHeader(tt.msgID, tt.fragNum, tt.fragTotal)
		gotID, gotNum, gotTotal := DecodeHeader(b)
		if gotID != tt.msgID || gotNum != tt.fragNum || gotTotal != tt.fragTotal {
			t.Errorf("EncodeHeader(%d,%d,%d)=0x%02x → DecodeHeader=(%d,%d,%d)",
				tt.msgID, tt.fragNum, tt.fragTotal, b, gotID, gotNum, gotTotal)
		}
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

	frags := Fragment(data, IridiumMO_MTU, 3)
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments for 500 bytes at MTU 340, got %d", len(frags))
	}

	// Verify headers.
	msgID, fragNum, fragTotal := DecodeHeader(frags[0][0])
	if msgID != 3 || fragNum != 0 || fragTotal != 2 {
		t.Errorf("frag 0 header: id=%d num=%d total=%d", msgID, fragNum, fragTotal)
	}
	msgID, fragNum, fragTotal = DecodeHeader(frags[1][0])
	if msgID != 3 || fragNum != 1 || fragTotal != 2 {
		t.Errorf("frag 1 header: id=%d num=%d total=%d", msgID, fragNum, fragTotal)
	}

	// Verify payload sizes.
	fragPayload := IridiumMO_MTU - HeaderSize
	if len(frags[0]) != fragPayload+HeaderSize {
		t.Errorf("frag 0 size: got %d, want %d", len(frags[0]), fragPayload+HeaderSize)
	}
	remainderPayload := 500 - fragPayload
	if len(frags[1]) != remainderPayload+HeaderSize {
		t.Errorf("frag 1 size: got %d, want %d", len(frags[1]), remainderPayload+HeaderSize)
	}
}

func TestFragment_Astrocast(t *testing.T) {
	data := make([]byte, 400) // 400 bytes, Astrocast MTU=160, payload=159
	frags := Fragment(data, AstrocastUL_MTU, 7)
	if len(frags) != 3 {
		t.Fatalf("expected 3 fragments for 400 bytes at MTU 160, got %d", len(frags))
	}
}

func TestFragment_MaxFragTruncation(t *testing.T) {
	// 2000 bytes at MTU 160 → needs 13 fragments, but max is 4
	data := make([]byte, 2000)
	frags := Fragment(data, AstrocastUL_MTU, 0)
	if len(frags) != MaxFragments {
		t.Fatalf("expected %d fragments (max), got %d", MaxFragments, len(frags))
	}
}

func TestFragment_MsgIDWrapping(t *testing.T) {
	data := make([]byte, 500)
	frags := Fragment(data, IridiumMO_MTU, 200) // 200 & 0x0F = 8
	msgID, _, _ := DecodeHeader(frags[0][0])
	if msgID != 8 {
		t.Errorf("expected msgID 8 (200 & 0x0F), got %d", msgID)
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
	_, err := r.AddFragment("dev1", []byte{})
	if err == nil {
		t.Error("expected error for empty fragment")
	}
}

func TestReassembly_InvalidFragNum(t *testing.T) {
	r := NewReassembler(5 * time.Minute)
	// Header with fragTotal=1 but fragNum=2 (invalid).
	header := EncodeHeader(0, 2, 1)
	_, err := r.AddFragment("dev1", []byte{header, 0x01})
	if err == nil {
		t.Error("expected error for fragNum >= fragTotal")
	}
}
