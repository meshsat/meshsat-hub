package tak

import (
	"bytes"
	"errors"
)

// Framer cuts the stream a TAK server sends into events (MESHSAT-1461).
//
// A TAK stream is events laid end to end with nothing between them that can be
// relied on: some servers put a newline after each, some an XML declaration
// before each, some nothing. So an event is found the way the servers themselves
// find one -- OpenTAKServer splits its input on the literal "</event>" -- by
// looking for where it starts and where it ends.
//
// That is deliberately all this does. It does not parse, and what it returns is
// not trusted: every frame goes through Sanitize, which is where a frame cut in
// the wrong place (a "</event>" inside a CDATA section, say) is refused. Framing
// only has to never grow without bound and never lose its place for good, and
// both hold: a frame is capped, and after anything it cannot use the framer
// simply looks for the next start.
//
// It reads bytes it is given. It owns no connection and sets no deadline: the
// caller reads with a plain conn.Read and unblocks it by closing the connection
// (rule 15: a deadline around a bufio.Scanner leaves the scanner unable to read
// again, and this is the read loop that must not do that).
type Framer struct {
	max  int
	buf  []byte
	in   bool // buf starts at the '<' of an event that has not ended yet
	scan int  // where the search for the end resumes
	junk int  // bytes skipped since the last event that were not white space
}

// maxJunk is how much may arrive between events that is neither an event nor
// white space. An XML declaration is a few dozen bytes; more than this is a
// stream that is not CoT XML at all.
const maxJunk = 4 << 10

var (
	// ErrFrameTooLarge is an event that did not end within the limit.
	ErrFrameTooLarge = errors.New("tak: an event in the stream is larger than the limit")
	// ErrNotCoTStream is a stream that is not CoT XML: TAK protocol version 1
	// frames, which begin with 0xBF, or anything else that is not events.
	ErrNotCoTStream = errors.New("tak: the stream is not CoT XML")
)

var (
	eventOpen  = []byte("<event")
	eventClose = []byte("</event>")
)

// NewFramer returns a framer that refuses an event larger than max bytes.
func NewFramer(max int) *Framer { return &Framer{max: max} }

// Feed takes the next bytes of the stream and returns the events they complete,
// each in a slice of its own. After an error the framer must not be used again:
// the stream has lost its meaning and the connection should be closed.
func (f *Framer) Feed(p []byte) ([][]byte, error) {
	f.buf = append(f.buf, p...)
	var out [][]byte
	for {
		if !f.in {
			start, err := f.findStart()
			if err != nil {
				return out, err
			}
			if start < 0 {
				return out, nil
			}
			f.buf = f.buf[start:]
			f.in, f.scan, f.junk = true, len(eventOpen), 0
		}
		i := bytes.Index(f.buf[f.scan:], eventClose)
		if i < 0 {
			if len(f.buf) > f.max {
				return out, ErrFrameTooLarge
			}
			// Resume a little before the end: the closing tag may be split across
			// two reads.
			if f.scan = len(f.buf) - len(eventClose) + 1; f.scan < len(eventOpen) {
				f.scan = len(eventOpen)
			}
			return out, nil
		}
		end := f.scan + i + len(eventClose)
		if end > f.max {
			return out, ErrFrameTooLarge
		}
		out = append(out, append([]byte(nil), f.buf[:end]...))
		f.buf = f.buf[end:]
		f.in, f.scan = false, 0
	}
}

// findStart returns the offset of the next event's '<' in buf, or -1 when there
// is none yet. What it skips is dropped from buf, apart from a tail that could be
// the beginning of "<event" cut by a read.
func (f *Framer) findStart() (int, error) {
	from := 0
	for {
		i := bytes.Index(f.buf[from:], eventOpen)
		if i < 0 {
			break
		}
		at := from + i
		after := at + len(eventOpen)
		if after >= len(f.buf) {
			// "<event" is the last thing here; the byte that says whether it is an
			// event or, say, <events> has not arrived.
			if err := f.skip(at); err != nil {
				return -1, err
			}
			return -1, nil
		}
		switch f.buf[after] {
		case ' ', '\t', '\r', '\n', '>', '/':
			if err := f.count(f.buf[:at]); err != nil {
				return -1, err
			}
			return at, nil
		}
		from = after
	}
	// No start. Keep only a tail that really is the beginning of "<event" cut by
	// a read; everything else is skipped and counted now, not held back.
	return -1, f.skip(len(f.buf) - partialOpen(f.buf))
}

// partialOpen is the length of the longest tail of buf that is a proper prefix
// of "<event".
func partialOpen(buf []byte) int {
	for n := len(eventOpen) - 1; n > 0; n-- {
		if n <= len(buf) && bytes.Equal(buf[len(buf)-n:], eventOpen[:n]) {
			return n
		}
	}
	return 0
}

// skip drops the first n bytes of buf, counting what they were.
func (f *Framer) skip(n int) error {
	if n <= 0 {
		return nil
	}
	if err := f.count(f.buf[:n]); err != nil {
		return err
	}
	f.buf = append(f.buf[:0], f.buf[n:]...)
	return nil
}

// count charges skipped bytes against the junk allowance.
func (f *Framer) count(skipped []byte) error {
	for _, c := range skipped {
		switch c {
		case ' ', '\t', '\r', '\n':
		case 0xBF:
			// Outside an event, 0xBF is the first byte of a TAK protocol
			// version 1 frame: the server is speaking protobuf, which this does
			// not. (Inside an event it is an ordinary UTF-8 continuation byte,
			// and this is never called on bytes inside one.)
			return ErrNotCoTStream
		default:
			if f.junk++; f.junk > maxJunk {
				return ErrNotCoTStream
			}
		}
	}
	return nil
}
