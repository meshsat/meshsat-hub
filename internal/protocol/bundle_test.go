package protocol

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"
)

func TestBundleHeaderMarshalRoundTrip(t *testing.T) {
	var bundleID [16]byte
	rand.Read(bundleID[:])

	hdr := &BundleHeader{
		Version:   BundleVersion,
		BundleID:  bundleID,
		FragIndex: 3,
		FragTotal: 10,
		TotalSize: 5000,
	}

	data := MarshalBundleHeader(hdr)
	if len(data) != BundleHeaderLen {
		t.Fatalf("expected %d bytes, got %d", BundleHeaderLen, len(data))
	}

	parsed, err := UnmarshalBundleHeader(data)
	if err != nil {
		t.Fatalf("UnmarshalBundleHeader: %v", err)
	}

	if parsed.Version != BundleVersion {
		t.Fatal("version mismatch")
	}
	if parsed.BundleID != bundleID {
		t.Fatal("bundle ID mismatch")
	}
	if parsed.FragIndex != 3 || parsed.FragTotal != 10 || parsed.TotalSize != 5000 {
		t.Fatal("field value mismatch")
	}
}

func TestBundleHeaderInvalid(t *testing.T) {
	// Too short.
	if _, err := UnmarshalBundleHeader(make([]byte, 10)); err == nil {
		t.Fatal("expected error for short data")
	}

	// Bad version.
	data := make([]byte, BundleHeaderLen)
	data[0] = 0xFF
	if _, err := UnmarshalBundleHeader(data); err == nil {
		t.Fatal("expected error for bad version")
	}
}

func TestFragmentBundleRoundTrip(t *testing.T) {
	payload := make([]byte, 1000)
	rand.Read(payload)

	mtu := 300
	bundleID, frags, err := FragmentBundle(payload, mtu)
	if err != nil {
		t.Fatalf("FragmentBundle: %v", err)
	}
	if len(frags) == 0 {
		t.Fatal("expected at least one fragment")
	}
	_ = bundleID

	// All fragments should be <= MTU.
	for i, f := range frags {
		if len(f) > mtu {
			t.Fatalf("fragment %d exceeds MTU: %d > %d", i, len(f), mtu)
		}
		if !IsBundleFragment(f) {
			t.Fatalf("fragment %d not detected as bundle fragment", i)
		}
	}

	// Reassemble.
	rb := NewBundleReassemblyBuffer(10*time.Second, 100)
	var result []byte
	for _, f := range frags {
		r, err := rb.Reassemble(f)
		if err != nil {
			t.Fatalf("Reassemble: %v", err)
		}
		if r != nil {
			result = r
		}
	}

	if result == nil {
		t.Fatal("expected reassembled result")
	}
	if !bytes.Equal(result, payload) {
		t.Fatal("reassembled payload mismatch")
	}
}

func TestFragmentBundleSingleFragment(t *testing.T) {
	payload := []byte("small")
	mtu := 500

	_, frags, err := FragmentBundle(payload, mtu)
	if err != nil {
		t.Fatalf("FragmentBundle: %v", err)
	}
	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(frags))
	}

	rb := NewBundleReassemblyBuffer(10*time.Second, 100)
	result, err := rb.Reassemble(frags[0])
	if err != nil {
		t.Fatalf("Reassemble: %v", err)
	}
	if !bytes.Equal(result, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestFragmentBundleMTUTooSmall(t *testing.T) {
	_, _, err := FragmentBundle([]byte("test"), BundleHeaderLen)
	if err == nil {
		t.Fatal("expected error for MTU <= header size")
	}
}

func TestBundleReassemblyBufferReap(t *testing.T) {
	rb := NewBundleReassemblyBuffer(1*time.Millisecond, 100)

	payload := make([]byte, 500)
	rand.Read(payload)
	_, frags, _ := FragmentBundle(payload, 100)

	// Add only the first fragment (incomplete).
	_, _ = rb.Reassemble(frags[0])
	if rb.PendingCount() != 1 {
		t.Fatalf("expected 1 pending, got %d", rb.PendingCount())
	}

	// Wait for expiry.
	time.Sleep(5 * time.Millisecond)
	reaped := rb.Reap()
	if reaped != 1 {
		t.Fatalf("expected 1 reaped, got %d", reaped)
	}
	if rb.PendingCount() != 0 {
		t.Fatalf("expected 0 pending after reap, got %d", rb.PendingCount())
	}
}

func TestBundleReassemblyBufferCapacity(t *testing.T) {
	rb := NewBundleReassemblyBuffer(10*time.Second, 1)

	// First bundle starts OK.
	payload1 := make([]byte, 500)
	rand.Read(payload1)
	_, frags1, _ := FragmentBundle(payload1, 100)
	_, err := rb.Reassemble(frags1[0])
	if err != nil {
		t.Fatalf("first bundle should succeed: %v", err)
	}

	// Second bundle should be rejected (buffer full).
	payload2 := make([]byte, 500)
	rand.Read(payload2)
	_, frags2, _ := FragmentBundle(payload2, 100)
	_, err = rb.Reassemble(frags2[0])
	if err == nil {
		t.Fatal("expected error for full buffer")
	}
}

func TestPendingBundleInfo(t *testing.T) {
	rb := NewBundleReassemblyBuffer(10*time.Second, 100)

	payload := make([]byte, 500)
	rand.Read(payload)
	bundleID, frags, _ := FragmentBundle(payload, 100)

	// Before any fragments.
	recv, total := rb.PendingBundleInfo(bundleID)
	if recv != 0 || total != 0 {
		t.Fatal("expected 0/0 for unknown bundle")
	}

	// After first fragment.
	_, _ = rb.Reassemble(frags[0])
	recv, total = rb.PendingBundleInfo(bundleID)
	if recv != 1 {
		t.Fatalf("expected 1 received, got %d", recv)
	}
	if total != len(frags) {
		t.Fatalf("expected total %d, got %d", len(frags), total)
	}
}
