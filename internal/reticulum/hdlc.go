package reticulum

// HDLC-like framing used by Reticulum's TCPInterface and SerialInterface.
// Wire-compatible with Python RNS (RNS/Interfaces/TCPInterface.py class HDLC)
// and MeshSat Bridge (internal/reticulum/hdlc.go).

const (
	// HDLCFlag is the frame delimiter byte.
	HDLCFlag byte = 0x7E
	// HDLCEsc is the escape byte (PPP-style, same as RFC 1662).
	HDLCEsc byte = 0x7D
	// HDLCEscMask is XORed with escaped bytes.
	HDLCEscMask byte = 0x20
)

// HDLCEscape escapes a raw payload for HDLC framing.
func HDLCEscape(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/4)
	for _, b := range data {
		switch b {
		case HDLCEsc:
			out = append(out, HDLCEsc, HDLCEsc^HDLCEscMask)
		case HDLCFlag:
			out = append(out, HDLCEsc, HDLCFlag^HDLCEscMask)
		default:
			out = append(out, b)
		}
	}
	return out
}

// HDLCUnescape reverses HDLC escaping.
func HDLCUnescape(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == HDLCEsc && i+1 < len(data) {
			out = append(out, data[i+1]^HDLCEscMask)
			i++
		} else {
			out = append(out, data[i])
		}
	}
	return out
}

// HDLCFrame wraps a raw Reticulum packet in HDLC framing: [FLAG][escaped_data][FLAG]
func HDLCFrame(data []byte) []byte {
	escaped := HDLCEscape(data)
	frame := make([]byte, 0, 2+len(escaped))
	frame = append(frame, HDLCFlag)
	frame = append(frame, escaped...)
	frame = append(frame, HDLCFlag)
	return frame
}

// MaxFrameBufferSize bounds the reassembly buffer of a single HDLCFrameReader.
//
// WHY THIS EXISTS (MESHSAT-1202). Feed() only discarded its buffer when the data
// contained NO delimiter at all. A peer that sent one 0x7E and then never a second one
// landed in the "no closing flag yet" branch, which KEEPS the partial frame — so every
// subsequent read appended to a buffer that nothing would ever drain. Growth was
// unbounded and chosen entirely by the remote side. The path is reachable from the
// internet: reticulum.meshsat.net:443 -> VPS SNI passthrough -> notrf01 relay :4243 ->
// stunnel -> TCPInterface.readLoop, which feeds straight into Feed(). The 60 s read
// deadline does not help, because a slow trickle resets it on every read.
//
// WHY THIS VALUE. A legitimate frame cannot approach it: MTU is 500 and HDLC escaping at
// worst doubles a payload, so the largest escaped frame on the wire is ~1002 bytes. 16 KiB
// is roughly sixteen times that ceiling, deliberately. The SOS invariant says nothing
// added for safety may drop a real packet, so the cap is set far enough above any frame
// this protocol can produce that the only thing it can ever discard is garbage — while
// still bounding memory to a fixed amount per connection instead of "whatever the peer
// feels like sending".
const MaxFrameBufferSize = 16384

// HDLCFrameReader extracts complete HDLC frames from a byte stream.
type HDLCFrameReader struct {
	buf []byte

	// oversizeDrops counts resyncs forced by MaxFrameBufferSize. A non-zero value means
	// a peer sent an opening delimiter and then more than MaxFrameBufferSize bytes
	// without closing it, which no conforming implementation does.
	oversizeDrops uint64
}

// NewHDLCFrameReader creates a new HDLC frame reader.
func NewHDLCFrameReader() *HDLCFrameReader {
	return &HDLCFrameReader{}
}

// OversizeDrops reports how many times this reader discarded an over-long partial frame.
func (r *HDLCFrameReader) OversizeDrops() uint64 { return r.oversizeDrops }

// Feed adds data to the buffer and returns any complete frames extracted.
func (r *HDLCFrameReader) Feed(data []byte) [][]byte {
	r.buf = append(r.buf, data...)

	var frames [][]byte
	for {
		start := -1
		for i, b := range r.buf {
			if b == HDLCFlag {
				start = i
				break
			}
		}
		if start < 0 {
			r.buf = r.buf[:0]
			break
		}

		end := -1
		for i := start + 1; i < len(r.buf); i++ {
			if r.buf[i] == HDLCFlag {
				end = i
				break
			}
		}
		if end < 0 {
			r.buf = r.buf[start:]
			// The partial frame is already longer than any frame this protocol can
			// produce, so it is not a frame. Resync: drop it and wait for the next
			// opening delimiter. r.buf is set to nil rather than r.buf[:0] so the
			// oversized backing array is actually released — reslicing to zero length
			// keeps the capacity, which would hand the attacker a permanent allocation
			// per connection even though the length is bounded.
			if len(r.buf) > MaxFrameBufferSize {
				r.buf = nil
				r.oversizeDrops++
			}
			break
		}

		escaped := r.buf[start+1 : end]
		if len(escaped) >= HeaderMinSize {
			frame := HDLCUnescape(escaped)
			frames = append(frames, frame)
		}

		r.buf = r.buf[end:]
	}

	return frames
}
