package ipougrs

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"sync"
	"time"
)

// Frame protocol constants.
const (
	// Magic is the first byte of every IPoUGRS frame, identifying it as
	// IP-over-satellite tunnel traffic (ASCII 'I').
	Magic byte = 0x49

	// HeaderSize is the fixed frame header size in bytes.
	HeaderSize = 4

	// MaxFragments is the maximum number of fragments per IP packet
	// (limited by 4-bit field).
	MaxFragments = 16

	// FlagCompressed indicates the payload is DEFLATE-compressed.
	// Compression is applied to the full IP packet before fragmentation,
	// so only the first fragment carries meaningful compression state —
	// all fragments of a compressed packet have this flag set.
	FlagCompressed byte = 0x01
)

// Frame represents a single IPoUGRS SBD frame.
type Frame struct {
	FragIndex uint8  // 0-based fragment index
	FragTotal uint8  // total number of fragments (1-16)
	PacketID  uint8  // wrapping counter per tunnel endpoint
	Flags     byte   // FlagCompressed, etc.
	Payload   []byte // IP packet fragment (optionally compressed)
}

// Encode serializes the frame into wire format.
func (f *Frame) Encode() []byte {
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0] = Magic
	buf[1] = (f.FragIndex & 0x0F << 4) | ((f.FragTotal - 1) & 0x0F)
	buf[2] = f.PacketID
	buf[3] = f.Flags
	copy(buf[HeaderSize:], f.Payload)
	return buf
}

// DecodeFrame parses wire bytes into a Frame.
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("ipougrs: frame too short: %d bytes", len(data))
	}
	if data[0] != Magic {
		return nil, fmt.Errorf("ipougrs: bad magic: 0x%02x (want 0x%02x)", data[0], Magic)
	}

	f := &Frame{
		FragIndex: (data[1] >> 4) & 0x0F,
		FragTotal: (data[1] & 0x0F) + 1,
		PacketID:  data[2],
		Flags:     data[3],
		Payload:   make([]byte, len(data)-HeaderSize),
	}
	copy(f.Payload, data[HeaderSize:])

	if f.FragIndex >= f.FragTotal {
		return nil, fmt.Errorf("ipougrs: frag_index %d >= frag_total %d", f.FragIndex, f.FragTotal)
	}

	return f, nil
}

// IsIPoUGRS returns true if data starts with the IPoUGRS magic byte and
// is long enough to contain a valid header.
func IsIPoUGRS(data []byte) bool {
	return len(data) >= HeaderSize && data[0] == Magic
}

// FragmentPacket splits an IP packet into IPoUGRS frames that fit within
// the given SBD MTU. If compress is true, the packet is DEFLATE-compressed
// before fragmentation. Returns nil if the packet is empty.
func FragmentPacket(packet []byte, sbdMTU int, packetID uint8, compress bool) ([]*Frame, error) {
	if len(packet) == 0 {
		return nil, nil
	}

	payload := packet
	var flags byte

	if compress {
		compressed, err := deflateCompress(packet)
		if err != nil {
			return nil, fmt.Errorf("ipougrs: compress: %w", err)
		}
		// Only use compressed form if it's actually smaller.
		if len(compressed) < len(packet) {
			payload = compressed
			flags |= FlagCompressed
		}
	}

	fragPayload := sbdMTU - HeaderSize
	if fragPayload <= 0 {
		return nil, fmt.Errorf("ipougrs: sbd_mtu %d too small for header", sbdMTU)
	}

	// No fragmentation needed.
	if len(payload) <= fragPayload {
		return []*Frame{{
			FragIndex: 0,
			FragTotal: 1,
			PacketID:  packetID,
			Flags:     flags,
			Payload:   payload,
		}}, nil
	}

	nFrags := (len(payload) + fragPayload - 1) / fragPayload
	if nFrags > MaxFragments {
		return nil, fmt.Errorf("ipougrs: packet too large: needs %d fragments (max %d)", nFrags, MaxFragments)
	}

	frames := make([]*Frame, 0, nFrags)
	for i := 0; i < nFrags; i++ {
		start := i * fragPayload
		end := start + fragPayload
		if end > len(payload) {
			end = len(payload)
		}
		frag := make([]byte, end-start)
		copy(frag, payload[start:end])

		frames = append(frames, &Frame{
			FragIndex: uint8(i),
			FragTotal: uint8(nFrags),
			PacketID:  packetID,
			Flags:     flags,
			Payload:   frag,
		})
	}

	return frames, nil
}

// pendingPacket holds fragments being reassembled into an IP packet.
type pendingPacket struct {
	fragments []*Frame
	total     int
	received  int
	flags     byte
	createdAt time.Time
}

// PacketReassembler collects IPoUGRS frames and reassembles complete IP packets.
// Thread-safe. Keyed by (deviceID, packetID) for per-device isolation.
type PacketReassembler struct {
	mu      sync.Mutex
	pending map[string]*pendingPacket // key: "deviceID:packetID"
	maxAge  time.Duration
}

// NewPacketReassembler creates a reassembler with the given expiry timeout.
func NewPacketReassembler(maxAge time.Duration) *PacketReassembler {
	return &PacketReassembler{
		pending: make(map[string]*pendingPacket),
		maxAge:  maxAge,
	}
}

// AddFrame adds a decoded frame. Returns the reassembled IP packet if all
// fragments are present, nil otherwise.
func (r *PacketReassembler) AddFrame(deviceID string, frame *Frame) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", deviceID, frame.PacketID)

	r.mu.Lock()
	defer r.mu.Unlock()

	pp, ok := r.pending[key]
	if !ok {
		pp = &pendingPacket{
			fragments: make([]*Frame, frame.FragTotal),
			total:     int(frame.FragTotal),
			flags:     frame.Flags,
			createdAt: time.Now(),
		}
		r.pending[key] = pp
	}

	if int(frame.FragIndex) >= pp.total {
		return nil, fmt.Errorf("ipougrs: frag_index %d >= total %d", frame.FragIndex, pp.total)
	}

	if pp.fragments[frame.FragIndex] == nil {
		pp.received++
	}
	pp.fragments[frame.FragIndex] = frame

	if pp.received < pp.total {
		return nil, nil // not yet complete
	}

	// All fragments received — reassemble.
	var totalLen int
	for _, f := range pp.fragments {
		totalLen += len(f.Payload)
	}
	assembled := make([]byte, 0, totalLen)
	for _, f := range pp.fragments {
		assembled = append(assembled, f.Payload...)
	}
	flags := pp.flags
	delete(r.pending, key)

	// Decompress if needed.
	if flags&FlagCompressed != 0 {
		decompressed, err := deflateDecompress(assembled)
		if err != nil {
			return nil, fmt.Errorf("ipougrs: decompress: %w", err)
		}
		return decompressed, nil
	}

	return assembled, nil
}

// Expire removes pending reassemblies older than maxAge.
func (r *PacketReassembler) Expire() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.maxAge)
	expired := 0
	for key, pp := range r.pending {
		if pp.createdAt.Before(cutoff) {
			delete(r.pending, key)
			expired++
		}
	}
	return expired
}

// PendingCount returns the number of packets awaiting reassembly.
func (r *PacketReassembler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// deflateCompress compresses data using DEFLATE (RFC 1951).
func deflateCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// deflateDecompress decompresses DEFLATE data.
func deflateDecompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer func() { _ = r.Close() }()
	// Limit decompressed size to prevent zip bombs (64 KB max for IP packets).
	return io.ReadAll(io.LimitReader(r, 65536))
}
