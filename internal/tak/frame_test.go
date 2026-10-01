package tak

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func feedAll(t *testing.T, f *Framer, chunks ...string) []string {
	t.Helper()
	var out []string
	for _, c := range chunks {
		frames, err := f.Feed([]byte(c))
		if err != nil {
			t.Fatalf("Feed(%q): %v", c, err)
		}
		for _, fr := range frames {
			out = append(out, string(fr))
		}
	}
	return out
}

const (
	evA = `<event uid="a" type="a-f-G-U-C"><point lat="1" lon="2"/><detail><contact callsign="é"/></detail></event>`
	evB = `<event uid="b" type="a-f-G-U-C"><point lat="3" lon="4"/></event>`
)

// However the stream is cut into reads, the same events come out.
func TestFramerFindsEventsHoweverTheStreamIsCut(t *testing.T) {
	stream := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" + evA + "\n\n" +
		"<?xml version='1.0'?>" + evB + evA + "\r\n"
	want := []string{evA, evB, evA}

	for _, size := range []int{1, 2, 3, 5, 7, 8, 13, 64, len(stream)} {
		f := NewFramer(64 << 10)
		var got []string
		for i := 0; i < len(stream); i += size {
			end := i + size
			if end > len(stream) {
				end = len(stream)
			}
			got = append(got, feedAll(t, f, stream[i:end])...)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("read size %d: got %d events, want %d\n%q", size, len(got), len(want), got)
		}
	}
}

// Each frame is a slice of its own: the framer reuses its buffer, and a caller
// holding an earlier frame must not see it change.
func TestFramerFramesDoNotShareMemory(t *testing.T) {
	f := NewFramer(64 << 10)
	first := feedAll(t, f, evA)
	keep := []byte(first[0])
	feedAll(t, f, evB+evB+evB)
	if string(keep) != evA {
		t.Errorf("an earlier frame changed after later reads:\n%s", keep)
	}
	frames, _ := f.Feed([]byte(evA + evB))
	if len(frames) == 2 && &frames[0][0] == &frames[1][0] {
		t.Error("two frames share a backing array")
	}
}

// Only <event ...> starts an event. <events>, <eventful> and text that mentions
// the word do not.
func TestFramerDoesNotMistakeOtherTagsForAnEvent(t *testing.T) {
	f := NewFramer(64 << 10)
	got := feedAll(t, f, "<events><eventful/>", "<event", "s/>", evB)
	if len(got) != 1 || got[0] != evB {
		t.Errorf("got %q, want only the real event", got)
	}
	// The forms an event start does take.
	for _, start := range []string{"<event>", "<event\n uid='x'>", "<event\tuid='x'>"} {
		f := NewFramer(64 << 10)
		if got := feedAll(t, f, start+"</event>"); len(got) != 1 {
			t.Errorf("%q did not start an event", start)
		}
	}
}

// An event that never ends must not grow without bound.
func TestFramerRefusesAnEventOverTheLimit(t *testing.T) {
	f := NewFramer(1024)
	_, err := f.Feed([]byte("<event uid='x'>" + strings.Repeat("y", 2000)))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("an unterminated 2000-byte event under a 1024 limit: %v", err)
	}
	// And one that does end, but past the limit.
	f = NewFramer(1024)
	_, err = f.Feed([]byte("<event uid='x'>" + strings.Repeat("y", 2000) + "</event>"))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("a terminated 2000-byte event under a 1024 limit: %v", err)
	}
	// Fed slowly, it is still caught, and before much more than the limit is held.
	f = NewFramer(1024)
	var ferr error
	fed := 0
	for i := 0; i < 5000 && ferr == nil; i++ {
		_, ferr = f.Feed([]byte("<event uid='x'>y"[min(i, 15):]))
		fed++
	}
	if !errors.Is(ferr, ErrFrameTooLarge) {
		t.Errorf("a slowly fed endless event was never refused: %v", ferr)
	}
}

// A server that switches to TAK protocol version 1 sends frames starting 0xBF.
// That is not something to hunt for events in.
func TestFramerRefusesAProtobufStream(t *testing.T) {
	f := NewFramer(64 << 10)
	if _, err := f.Feed([]byte{0xBF, 0x10, 0xBF, 0x0A, 0x02}); !errors.Is(err, ErrNotCoTStream) {
		t.Errorf("a protobuf frame: %v", err)
	}
	// After an event, too.
	f = NewFramer(64 << 10)
	if _, err := f.Feed(append([]byte(evA), 0xBF, 0x10)); !errors.Is(err, ErrNotCoTStream) {
		t.Errorf("a protobuf frame after an event: %v", err)
	}
	// But 0xBF INSIDE an event is an ordinary UTF-8 continuation byte ("¿" is
	// C2 BF), and must not be taken for one.
	f = NewFramer(64 << 10)
	ev := `<event uid="q"><point lat="1" lon="2"/><detail><remarks>¿dónde?</remarks></detail></event>`
	if got := feedAll(t, f, ev); len(got) != 1 || got[0] != ev {
		t.Errorf("an event containing the byte 0xBF was not framed: %q", got)
	}
}

// Something that is simply not CoT is given up on, not searched for ever.
func TestFramerGivesUpOnAStreamThatIsNotEvents(t *testing.T) {
	f := NewFramer(64 << 10)
	var err error
	for i := 0; i < 100 && err == nil; i++ {
		_, err = f.Feed([]byte(strings.Repeat("HTTP/1.1 400 Bad Request ", 10)))
	}
	if !errors.Is(err, ErrNotCoTStream) {
		t.Errorf("2.5 kB x 100 of something else was never given up on: %v", err)
	}
	// White space between events is not junk, however much of it there is.
	f = NewFramer(64 << 10)
	if _, err := f.Feed([]byte(strings.Repeat(" \r\n\t", 5000) + evA)); err != nil {
		t.Errorf("white space between events was counted as junk: %v", err)
	}
	// Nor does a declaration before every event add up.
	f = NewFramer(64 << 10)
	for i := 0; i < 500; i++ {
		if _, err := f.Feed([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + evB)); err != nil {
			t.Fatalf("event %d with its declaration: %v", i, err)
		}
	}
}

// The framer never holds more than the limit plus one read, and never returns a
// frame that does not start and end as an event.
func FuzzFramer(f *testing.F) {
	f.Add([]byte(evA+evB), 3)
	f.Add([]byte("<event"), 1)
	f.Add([]byte("</event><event></event>"), 2)
	f.Add([]byte{0xBF, 0x01}, 1)
	f.Add([]byte("<event><![CDATA[</event>]]></event>"), 4)
	f.Fuzz(func(t *testing.T, stream []byte, size int) {
		if size <= 0 || size > 4096 {
			size = 7
		}
		const limit = 512
		fr := NewFramer(limit)
		for i := 0; i < len(stream); i += size {
			end := i + size
			if end > len(stream) {
				end = len(stream)
			}
			frames, err := fr.Feed(stream[i:end])
			for _, frame := range frames {
				if !bytes.HasPrefix(frame, []byte("<event")) || !bytes.HasSuffix(frame, []byte("</event>")) {
					t.Fatalf("a frame that is not an event: %q", frame)
				}
				if len(frame) > limit {
					t.Fatalf("a %d-byte frame under a %d limit", len(frame), limit)
				}
			}
			if err != nil {
				return
			}
			if len(fr.buf) > limit+size {
				t.Fatalf("holding %d bytes under a %d limit and %d-byte reads", len(fr.buf), limit, size)
			}
		}
	})
}
