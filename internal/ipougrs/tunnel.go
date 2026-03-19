// Package ipougrs implements IP-over-Unreliable-Ground-to-Relay-Satellite,
// an experimental IP tunnel over SBD satellite messages.
//
// EXPERIMENTAL — marked as alpha. Requires bridge-side counterpart in meshsat repo.
//
// Architecture:
//   - Hub side: receives IP packets from MQTT topic, fragments into SBD-sized
//     frames, sends via MT. Receives MO fragments, reassembles into IP packets,
//     publishes to MQTT topic for local TUN device or routing.
//   - Bridge side: TUN device captures IP packets, fragments, sends via MO.
//     Receives MT fragments, reassembles, injects into TUN device.
//
// Frame format:
//
//	Byte 0:    0x49 ('I') — IPoUGRS magic byte
//	Byte 1:    [4-bit frag_index | 4-bit frag_total]
//	Byte 2:    session_id (uint8, wrapping)
//	Byte 3-4:  total_length (uint16 BE, original IP packet length)
//	Bytes 5+:  payload fragment
//
// Constraints:
//   - MO MTU: 340 bytes → 335 bytes payload per fragment
//   - MT MTU: 270 bytes → 265 bytes payload per fragment
//   - Max IP packet: 16 fragments × 265 bytes = 4240 bytes (MT direction)
//   - Compression: optional zlib on IP payload before fragmentation
package ipougrs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MagicByte    = 0x49 // 'I' for IPoUGRS
	HeaderSize   = 5    // magic + frag_header + session_id + total_length(2)
	MaxFragments = 16
	MOPayloadMax = 340 - HeaderSize // 335
	MTPayloadMax = 270 - HeaderSize // 265
)

// Config holds IPoUGRS tunnel configuration.
type Config struct {
	Subnet      string        `json:"subnet"`       // e.g., "10.99.0.0/24"
	MTU         int           `json:"mtu"`          // tunnel MTU (default 1280 for IPv6-minimum)
	Compress    bool          `json:"compress"`     // zlib compression before fragmentation
	FragTimeout time.Duration `json:"frag_timeout"` // reassembly timeout (default 5 min)
	Enabled     bool          `json:"enabled"`
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Subnet:      "10.99.0.0/24",
		MTU:         1280,
		Compress:    true,
		FragTimeout: 5 * time.Minute,
		Enabled:     false, // opt-in (experimental)
	}
}

// Frame is a single IPoUGRS fragment.
type Frame struct {
	Magic       byte
	FragIndex   uint8
	FragTotal   uint8
	SessionID   uint8
	TotalLength uint16
	Payload     []byte
}

// EncodeFrame serializes a frame to bytes.
func EncodeFrame(f *Frame) []byte {
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0] = MagicByte
	buf[1] = (f.FragIndex&0x0F)<<4 | ((f.FragTotal - 1) & 0x0F)
	buf[2] = f.SessionID
	binary.BigEndian.PutUint16(buf[3:5], f.TotalLength)
	copy(buf[HeaderSize:], f.Payload)
	return buf
}

// DecodeFrame parses a frame from bytes.
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("ipougrs: frame too short (%d bytes)", len(data))
	}
	if data[0] != MagicByte {
		return nil, fmt.Errorf("ipougrs: invalid magic byte 0x%02X", data[0])
	}

	return &Frame{
		Magic:       data[0],
		FragIndex:   data[1] >> 4,
		FragTotal:   (data[1] & 0x0F) + 1,
		SessionID:   data[2],
		TotalLength: binary.BigEndian.Uint16(data[3:5]),
		Payload:     data[HeaderSize:],
	}, nil
}

// IsIPoUGRS returns true if the data starts with the IPoUGRS magic byte.
func IsIPoUGRS(data []byte) bool {
	return len(data) >= HeaderSize && data[0] == MagicByte
}

// Fragment splits an IP packet into IPoUGRS frames.
func Fragment(packet []byte, maxPayload int, sessionID uint8, compress bool) ([]Frame, error) {
	payload := packet
	if compress {
		compressed, err := zlibCompress(packet)
		if err == nil && len(compressed) < len(packet) {
			payload = compressed
		}
	}

	totalLen := uint16(len(packet)) // original uncompressed length

	nFrags := (len(payload) + maxPayload - 1) / maxPayload
	if nFrags > MaxFragments {
		return nil, fmt.Errorf("ipougrs: packet too large (%d bytes, max %d fragments × %d = %d)",
			len(payload), MaxFragments, maxPayload, MaxFragments*maxPayload)
	}
	if nFrags == 0 {
		nFrags = 1
	}

	frames := make([]Frame, 0, nFrags)
	for i := 0; i < nFrags; i++ {
		start := i * maxPayload
		end := start + maxPayload
		if end > len(payload) {
			end = len(payload)
		}

		frames = append(frames, Frame{
			Magic:       MagicByte,
			FragIndex:   uint8(i),
			FragTotal:   uint8(nFrags),
			SessionID:   sessionID,
			TotalLength: totalLen,
			Payload:     payload[start:end],
		})
	}

	return frames, nil
}

// Reassembler collects IPoUGRS fragments and reassembles IP packets.
type Reassembler struct {
	mu      sync.Mutex
	pending map[string]*pendingPacket
	maxAge  time.Duration
}

type pendingPacket struct {
	fragments   [][]byte
	total       int
	received    int
	totalLength uint16
	createdAt   time.Time
}

// NewReassembler creates an IPoUGRS fragment reassembler.
func NewReassembler(maxAge time.Duration) *Reassembler {
	return &Reassembler{
		pending: make(map[string]*pendingPacket),
		maxAge:  maxAge,
	}
}

// AddFrame adds a frame. Returns the reassembled IP packet if complete, nil otherwise.
func (r *Reassembler) AddFrame(deviceID string, frame *Frame) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", deviceID, frame.SessionID)

	r.mu.Lock()
	defer r.mu.Unlock()

	pp, ok := r.pending[key]
	if !ok {
		pp = &pendingPacket{
			fragments:   make([][]byte, frame.FragTotal),
			total:       int(frame.FragTotal),
			totalLength: frame.TotalLength,
			createdAt:   time.Now(),
		}
		r.pending[key] = pp
	}

	if int(frame.FragIndex) >= pp.total {
		return nil, fmt.Errorf("ipougrs: fragment index %d >= total %d", frame.FragIndex, pp.total)
	}

	if pp.fragments[frame.FragIndex] == nil {
		pp.received++
	}
	pp.fragments[frame.FragIndex] = frame.Payload

	if pp.received < pp.total {
		return nil, nil
	}

	// All fragments received — reassemble.
	var buf bytes.Buffer
	for _, f := range pp.fragments {
		buf.Write(f)
	}
	delete(r.pending, key)

	// Try zlib decompression (if compressed, result will be totalLength bytes).
	data := buf.Bytes()
	if len(data) < int(pp.totalLength) {
		decompressed, err := zlibDecompress(data)
		if err == nil {
			data = decompressed
		}
	}

	return data, nil
}

// Expire removes stale pending reassemblies.
func (r *Reassembler) Expire() int {
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

// Stats returns tunnel statistics.
type Stats struct {
	PacketsTx     uint64 `json:"packets_tx"`
	PacketsRx     uint64 `json:"packets_rx"`
	BytesTx       uint64 `json:"bytes_tx"`
	BytesRx       uint64 `json:"bytes_rx"`
	FragmentsTx   uint64 `json:"fragments_tx"`
	FragmentsRx   uint64 `json:"fragments_rx"`
	ReassemblyOK  uint64 `json:"reassembly_ok"`
	ReassemblyErr uint64 `json:"reassembly_err"`
}

// Tunnel manages the IPoUGRS tunnel state and statistics.
type Tunnel struct {
	config      Config
	reassembler *Reassembler
	sessionSeq  atomic.Uint32
	stats       Stats
	mu          sync.Mutex
}

// NewTunnel creates a new IPoUGRS tunnel.
func NewTunnel(cfg Config) *Tunnel {
	return &Tunnel{
		config:      cfg,
		reassembler: NewReassembler(cfg.FragTimeout),
	}
}

// NextSessionID returns the next wrapping session ID.
func (t *Tunnel) NextSessionID() uint8 {
	return uint8(t.sessionSeq.Add(1))
}

// FragmentPacket splits an IP packet into SBD frames for transmission.
func (t *Tunnel) FragmentPacket(packet []byte, direction string) ([][]byte, error) {
	maxPayload := MOPayloadMax
	if direction == "mt" {
		maxPayload = MTPayloadMax
	}

	frames, err := Fragment(packet, maxPayload, t.NextSessionID(), t.config.Compress)
	if err != nil {
		return nil, err
	}

	var encoded [][]byte
	for i := range frames {
		encoded = append(encoded, EncodeFrame(&frames[i]))
	}

	t.mu.Lock()
	t.stats.PacketsTx++
	t.stats.BytesTx += uint64(len(packet))
	t.stats.FragmentsTx += uint64(len(frames))
	t.mu.Unlock()

	slog.Debug("ipougrs: fragmented packet",
		"direction", direction, "bytes", len(packet), "fragments", len(frames))

	return encoded, nil
}

// ReassembleFrame processes an incoming IPoUGRS frame.
func (t *Tunnel) ReassembleFrame(deviceID string, data []byte) ([]byte, error) {
	frame, err := DecodeFrame(data)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.stats.FragmentsRx++
	t.mu.Unlock()

	packet, err := t.reassembler.AddFrame(deviceID, frame)
	if err != nil {
		t.mu.Lock()
		t.stats.ReassemblyErr++
		t.mu.Unlock()
		return nil, err
	}

	if packet != nil {
		t.mu.Lock()
		t.stats.PacketsRx++
		t.stats.BytesRx += uint64(len(packet))
		t.stats.ReassemblyOK++
		t.mu.Unlock()

		slog.Debug("ipougrs: reassembled packet", "device", deviceID, "bytes", len(packet))
	}

	return packet, nil
}

// GetStats returns current tunnel statistics.
func (t *Tunnel) GetStats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// GetConfig returns the tunnel configuration.
func (t *Tunnel) GetConfig() Config {
	return t.config
}

func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func zlibDecompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
