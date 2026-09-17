package reticulum

import (
	"bytes"
	"testing"
)

// The attack MESHSAT-1202 closed: one opening delimiter, then data forever and no closing
// delimiter. Before the cap, r.buf grew by every byte fed and nothing ever drained it, so
// the peer chose the Hub's memory usage. The assertion is on the BUFFER, not on the frame
// output — the old code also returned no frames here, which is exactly why the defect was
// invisible from the outside.
func TestFeedBoundsAnUnterminatedFrame(t *testing.T) {
	r := NewHDLCFrameReader()

	// Open a frame and never close it.
	if frames := r.Feed([]byte{HDLCFlag}); len(frames) != 0 {
		t.Fatalf("opening delimiter alone yielded %d frames, want 0", len(frames))
	}

	chunk := bytes.Repeat([]byte{0x41}, 4096) // no HDLCFlag, no HDLCEsc
	const rounds = 512                        // 2 MiB of attacker data
	for i := 0; i < rounds; i++ {
		if frames := r.Feed(chunk); len(frames) != 0 {
			t.Fatalf("round %d: unterminated data yielded %d frames, want 0", i, len(frames))
		}
		if got := len(r.buf); got > MaxFrameBufferSize {
			t.Fatalf("round %d: buffer length %d exceeds cap %d", i, got, MaxFrameBufferSize)
		}
		if got := cap(r.buf); got > 2*MaxFrameBufferSize+len(chunk) {
			t.Fatalf("round %d: buffer CAPACITY %d retained beyond the cap — reslicing to "+
				"zero length would do this; the fix must release the array", i, got)
		}
	}

	if r.OversizeDrops() == 0 {
		t.Fatal("fed 2 MiB with no closing delimiter and no oversize drop was recorded")
	}

	// The reader must still work afterwards: a resync is not a broken connection.
	payload := bytes.Repeat([]byte{0x42}, HeaderMinSize)
	frames := r.Feed(HDLCFrame(payload))
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("reader did not resync after the drop: got %d frames %v", len(frames), frames)
	}
}

// The cap must never be able to discard a real packet — the SOS invariant. The largest
// thing this protocol can put on the wire is an MTU-sized payload whose every byte needs
// escaping, which doubles it. That must pass with room to spare.
func TestCapCannotDropALegitimateFrame(t *testing.T) {
	// Every byte is HDLCEsc, so HDLCEscape emits two bytes for each one: the worst case.
	worst := bytes.Repeat([]byte{HDLCEsc}, MTU)
	wire := HDLCFrame(worst)

	if len(wire) > MaxFrameBufferSize {
		t.Fatalf("worst-case legitimate frame is %d bytes, which the %d cap would DROP",
			len(wire), MaxFrameBufferSize)
	}

	// Feed it one byte at a time — the reader holds the whole partial frame while doing so,
	// which is the state the cap is measured against.
	r := NewHDLCFrameReader()
	var got [][]byte
	for _, b := range wire {
		got = append(got, r.Feed([]byte{b})...)
	}
	if len(got) != 1 {
		t.Fatalf("worst-case frame fed bytewise produced %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0], worst) {
		t.Fatal("worst-case frame did not round-trip")
	}
	if r.OversizeDrops() != 0 {
		t.Fatalf("a legitimate frame triggered %d oversize drop(s)", r.OversizeDrops())
	}
}

// Whatever the input, the buffer stays bounded and Feed never panics. The codebase parses
// untrusted binary off an internet-reachable port and had no fuzz coverage at all.
//
// ⚠ THE GENERATOR SHAPE MATTERS, and a naive one proves nothing here. Feeding the same
// bytes over and over does NOT reach the cap: the repeat re-supplies a closing delimiter,
// so the reader finds a complete frame, emits it, and settles at a steady state. Measured
// while writing this — an earlier version of this target seeded
// `[FLAG] + 0x7D*1024` and passed happily with the bound removed, because from the second
// iteration onwards that input closes its own previous frame. The defect needs the opening
// delimiter to arrive ONCE and never again, so the second phase below strips delimiters out
// of the fuzz input before replaying it.
func FuzzHDLCFrameReader(f *testing.F) {
	f.Add([]byte{HDLCFlag})
	f.Add([]byte{HDLCFlag, HDLCEsc})
	f.Add([]byte{0x41, 0x42, 0x43})
	f.Add(HDLCFrame(bytes.Repeat([]byte{0x41}, HeaderMinSize)))
	f.Add(append([]byte{HDLCFlag}, bytes.Repeat([]byte{0x7D}, 1024)...))

	check := func(t *testing.T, r *HDLCFrameReader, where string, data []byte) {
		t.Helper()
		if got := len(r.buf); got > MaxFrameBufferSize {
			t.Fatalf("%s: buffer %d exceeds cap %d for input %q", where, got, MaxFrameBufferSize, data)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Phase 1 — arbitrary bytes, one at a time, so every partial-frame state is visited.
		r := NewHDLCFrameReader()
		for _, b := range data {
			r.Feed([]byte{b})
			check(t, r, "bytewise", data)
		}

		// Phase 2 — the unterminated-frame path: open a frame once, then replay the input
		// with every delimiter removed so it can never be closed.
		noFlag := bytes.ReplaceAll(data, []byte{HDLCFlag}, nil)
		if len(noFlag) == 0 {
			return
		}
		r2 := NewHDLCFrameReader()
		r2.Feed([]byte{HDLCFlag})
		for i := 0; i < 64; i++ {
			r2.Feed(noFlag)
			check(t, r2, "unterminated", data)
		}
	})
}
