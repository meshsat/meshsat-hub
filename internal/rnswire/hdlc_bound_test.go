package rnswire

import (
	"bytes"
	"testing"
)

// A peer that sends an opening flag and then never closes the frame must not
// grow the reader without bound, and framing must resume once it does send
// a complete frame (MESHSAT-1555: the Hub's listener faces the internet).
func TestHDLCReader_BoundedWhileFrameStaysOpen(t *testing.T) {
	r := NewHDLCFrameReader()
	r.Feed([]byte{HDLCFlag})
	chunk := bytes.Repeat([]byte{0x41}, 64*1024)
	for i := 0; i < 40; i++ { // 2.5 MiB without a closing flag
		if got := r.Feed(chunk); len(got) != 0 {
			t.Fatalf("frames out of an unterminated stream: %d", len(got))
		}
		if len(r.buf) > HDLCMaxBuffered {
			t.Fatalf("reader holds %d bytes, bound is %d", len(r.buf), HDLCMaxBuffered)
		}
	}
	payload := make([]byte, HeaderMinSize+8)
	for i := range payload {
		payload[i] = byte(i)
	}
	frames := r.Feed(HDLCFrame(payload))
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("framing did not resume after the flood: %d frames", len(frames))
	}
}

// The bound leaves the largest legitimate TCP frame intact: a full HW_MTU
// payload of flag bytes escapes to twice its size and still arrives whole.
func TestHDLCReader_LargestTCPFrameSurvivesTheBound(t *testing.T) {
	r := NewHDLCFrameReader()
	payload := bytes.Repeat([]byte{HDLCFlag}, 262144)
	frame := HDLCFrame(payload)
	var frames [][]byte
	for i := 0; i < len(frame); i += 4096 {
		end := i + 4096
		if end > len(frame) {
			end = len(frame)
		}
		frames = append(frames, r.Feed(frame[i:end])...)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("largest frame: got %d frames", len(frames))
	}
}
