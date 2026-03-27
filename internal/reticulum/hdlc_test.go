package reticulum

import (
	"bytes"
	"testing"
)

func TestHDLCEscapeUnescape(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"no special bytes", []byte{0x01, 0x02, 0x03}},
		{"contains flag", []byte{0x01, HDLCFlag, 0x03}},
		{"contains escape", []byte{0x01, HDLCEsc, 0x03}},
		{"both special", []byte{HDLCFlag, HDLCEsc, HDLCFlag}},
		{"all flags", []byte{HDLCFlag, HDLCFlag, HDLCFlag}},
		{"empty", []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escaped := HDLCEscape(tt.raw)
			// Escaped should not contain raw flag or escape bytes (except as escape sequences)
			for i, b := range escaped {
				if b == HDLCFlag {
					t.Errorf("escaped contains raw FLAG at position %d", i)
				}
			}
			unescaped := HDLCUnescape(escaped)
			if !bytes.Equal(unescaped, tt.raw) {
				t.Errorf("round-trip failed: got %x, want %x", unescaped, tt.raw)
			}
		})
	}
}

func TestHDLCFrame(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	frame := HDLCFrame(data)

	if frame[0] != HDLCFlag {
		t.Errorf("frame should start with FLAG, got %02x", frame[0])
	}
	if frame[len(frame)-1] != HDLCFlag {
		t.Errorf("frame should end with FLAG, got %02x", frame[len(frame)-1])
	}
}

func TestHDLCFrameReader(t *testing.T) {
	// Build a valid Reticulum-sized packet (>= HeaderMinSize bytes)
	packet := make([]byte, 20)
	for i := range packet {
		packet[i] = byte(i + 1)
	}
	frame := HDLCFrame(packet)

	reader := NewHDLCFrameReader()
	frames := reader.Feed(frame)

	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], packet) {
		t.Errorf("frame content: got %x, want %x", frames[0], packet)
	}
}

func TestHDLCFrameReader_Streaming(t *testing.T) {
	packet := make([]byte, 25)
	for i := range packet {
		packet[i] = byte(i)
	}
	frame := HDLCFrame(packet)

	reader := NewHDLCFrameReader()

	// Feed half the frame
	mid := len(frame) / 2
	frames := reader.Feed(frame[:mid])
	if len(frames) != 0 {
		t.Fatalf("expected 0 frames from partial data, got %d", len(frames))
	}

	// Feed the rest
	frames = reader.Feed(frame[mid:])
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame after completing data, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], packet) {
		t.Errorf("frame content: got %x, want %x", frames[0], packet)
	}
}

func TestHDLCFrameReader_MultipleFrames(t *testing.T) {
	p1 := make([]byte, 20)
	p2 := make([]byte, 22)
	for i := range p1 {
		p1[i] = byte(i + 10)
	}
	for i := range p2 {
		p2[i] = byte(i + 50)
	}

	// Concatenate two frames
	var combined []byte
	combined = append(combined, HDLCFrame(p1)...)
	combined = append(combined, HDLCFrame(p2)...)

	reader := NewHDLCFrameReader()
	frames := reader.Feed(combined)

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], p1) {
		t.Errorf("frame 0: got %x, want %x", frames[0], p1)
	}
	if !bytes.Equal(frames[1], p2) {
		t.Errorf("frame 1: got %x, want %x", frames[1], p2)
	}
}

func TestHDLCFrameReader_TooSmall(t *testing.T) {
	// Packet smaller than HeaderMinSize should be discarded
	small := make([]byte, 5)
	frame := []byte{HDLCFlag}
	frame = append(frame, HDLCEscape(small)...)
	frame = append(frame, HDLCFlag)

	reader := NewHDLCFrameReader()
	frames := reader.Feed(frame)

	if len(frames) != 0 {
		t.Errorf("expected 0 frames for undersized packet, got %d", len(frames))
	}
}
