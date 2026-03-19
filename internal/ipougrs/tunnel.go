package ipougrs

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// Stats holds tunnel traffic statistics.
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
// Inspired by draft-papadopoulos-ipougrs-00 (IP over Unanswered GSM Ring
// Signals) — but substitutes satellite SBD as the physical layer.
type Tunnel struct {
	config      Config
	reassembler *PacketReassembler
	packetSeq   atomic.Uint32
	stats       Stats
	mu          sync.Mutex
}

// NewTunnel creates a new IPoUGRS tunnel.
func NewTunnel(cfg Config) *Tunnel {
	return &Tunnel{
		config:      cfg,
		reassembler: NewPacketReassembler(cfg.FragTimeout),
	}
}

// NextPacketID returns the next wrapping packet ID.
func (t *Tunnel) NextPacketID() uint8 {
	return uint8(t.packetSeq.Add(1))
}

// FragmentForSend splits an IP packet into SBD frames for transmission.
func (t *Tunnel) FragmentForSend(packet []byte, sbdMTU int) ([][]byte, error) {
	frames, err := FragmentPacket(packet, sbdMTU, t.NextPacketID(), t.config.Compress)
	if err != nil {
		return nil, err
	}

	var encoded [][]byte
	for _, f := range frames {
		encoded = append(encoded, f.Encode())
	}

	t.mu.Lock()
	t.stats.PacketsTx++
	t.stats.BytesTx += uint64(len(packet))
	t.stats.FragmentsTx += uint64(len(frames))
	t.mu.Unlock()

	slog.Debug("ipougrs: fragmented packet", "bytes", len(packet), "fragments", len(frames))
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
