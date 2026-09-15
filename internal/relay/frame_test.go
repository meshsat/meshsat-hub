package relay

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	frame, err := EncodeEnvelope("phone-1", []byte{0, 1, 2, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame[:2], []byte{0x01, 7}) {
		t.Fatalf("header %x", frame[:2])
	}
	id, payload, err := DecodeEnvelope(frame)
	if err != nil || id != "phone-1" || !bytes.Equal(payload, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("id=%q payload=%x err=%v", id, payload, err)
	}
	// An empty payload is a legal frame; an empty id is not.
	if _, _, err := DecodeEnvelope(mustEnvelope(t, "x", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeEnvelope("", []byte("p")); !errors.Is(err, ErrClientID) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := EncodeEnvelope(strings.Repeat("a", 256), nil); !errors.Is(err, ErrClientID) {
		t.Fatalf("256-byte id: %v", err)
	}
}

func TestEnvelopeRefusesWhatItCannotParse(t *testing.T) {
	for _, bad := range [][]byte{nil, {0x01}, {0x02, 1, 'a'}, {0x01, 0}, {0x01, 5, 'a', 'b'}} {
		if _, _, err := DecodeEnvelope(bad); !errors.Is(err, ErrEnvelope) {
			t.Errorf("%x: %v", bad, err)
		}
	}
}

func mustEnvelope(t *testing.T, id string, p []byte) []byte {
	t.Helper()
	f, err := EncodeEnvelope(id, p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMemoryBudgetIsAFixedWindowPerKey(t *testing.T) {
	b := NewMemoryBudget()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !b.Allow("t:c1", 3) {
			t.Fatalf("frame %d refused", i)
		}
	}
	if b.Allow("t:c1", 3) {
		t.Fatal("fourth frame allowed")
	}
	if !b.Allow("t:c2", 3) {
		t.Fatal("another client's budget was spent")
	}
	now = now.Add(time.Minute)
	if !b.Allow("t:c1", 3) {
		t.Fatal("the next minute did not reset the window")
	}
	if !b.Allow("t:c1", 0) {
		t.Fatal("limit 0 must mean unlimited")
	}
}
