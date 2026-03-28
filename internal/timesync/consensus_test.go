package timesync

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// mockIdentity implements IdentityProvider for testing.
type mockIdentity struct {
	hash [DestHashLen]byte
}

func (m *mockIdentity) DestHash() [DestHashLen]byte {
	return m.hash
}

// packetCapture captures packets sent via SendFunc.
type packetCapture struct {
	mu      sync.Mutex
	packets [][]byte
}

func (pc *packetCapture) send(data []byte) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	pc.packets = append(pc.packets, cp)
}

func (pc *packetCapture) count() int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return len(pc.packets)
}

func (pc *packetCapture) last() []byte {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if len(pc.packets) == 0 {
		return nil
	}
	return pc.packets[len(pc.packets)-1]
}

func TestMeshTimeConsensusSendRequest(t *testing.T) {
	ts := NewTimeService(nil)
	id := &mockIdentity{}
	id.hash[0] = 0xAA
	pc := &packetCapture{}

	mc := NewMeshTimeConsensus(ts, id, pc.send)
	mc.sendRequest()

	if pc.count() != 1 {
		t.Fatalf("expected 1 packet, got %d", pc.count())
	}

	pkt := pc.last()
	if len(pkt) != 26 {
		t.Fatalf("expected 26-byte request, got %d", len(pkt))
	}
	if pkt[0] != PacketTimeSyncReq {
		t.Fatalf("expected type 0x%02x, got 0x%02x", PacketTimeSyncReq, pkt[0])
	}
	if pkt[1] != 0xAA {
		t.Fatalf("expected dest hash starting with 0xAA")
	}
}

func TestMeshTimeConsensusHandleRequestResponse(t *testing.T) {
	// Create two consensus instances simulating peer communication.
	tsA := NewTimeService(nil)
	idA := &mockIdentity{}
	idA.hash[0] = 0xAA
	pcA := &packetCapture{}
	mcA := NewMeshTimeConsensus(tsA, idA, pcA.send)

	tsB := NewTimeService(nil)
	idB := &mockIdentity{}
	idB.hash[0] = 0xBB
	pcB := &packetCapture{}
	mcB := NewMeshTimeConsensus(tsB, idB, pcB.send)

	// A sends a request.
	mcA.sendRequest()
	reqPkt := pcA.last()

	// B handles A's request and sends a response.
	mcB.HandleTimeSyncRequest(reqPkt, "test_iface")

	if pcB.count() != 1 {
		t.Fatalf("expected B to send 1 response, got %d", pcB.count())
	}

	respPkt := pcB.last()
	if respPkt[0] != PacketTimeSyncResp {
		t.Fatalf("expected response type 0x%02x, got 0x%02x", PacketTimeSyncResp, respPkt[0])
	}

	// A handles B's response.
	mcA.HandleTimeSyncResponse(respPkt)

	// A should now have 1 peer.
	if mcA.PeerCount() != 1 {
		t.Fatalf("expected 1 peer, got %d", mcA.PeerCount())
	}
}

func TestMeshTimeConsensusHandleUnknownResponse(t *testing.T) {
	ts := NewTimeService(nil)
	id := &mockIdentity{}
	pc := &packetCapture{}
	mc := NewMeshTimeConsensus(ts, id, pc.send)

	// Fabricate a response with no matching request.
	resp := make([]byte, 34)
	resp[0] = PacketTimeSyncResp
	binary.LittleEndian.PutUint64(resp[17:25], uint64(time.Now().UnixNano()))
	resp[25] = 1 // stratum
	binary.LittleEndian.PutUint64(resp[26:34], 12345)

	// Should not panic or update peer count.
	mc.HandleTimeSyncResponse(resp)
	if mc.PeerCount() != 0 {
		t.Fatal("expected 0 peers for unmatched response")
	}
}

func TestMeshTimeConsensusPeerCount(t *testing.T) {
	ts := NewTimeService(nil)
	id := &mockIdentity{}
	pc := &packetCapture{}
	mc := NewMeshTimeConsensus(ts, id, pc.send)

	if mc.PeerCount() != 0 {
		t.Fatal("expected 0 peers initially")
	}
}

func TestMeshTimeConsensusShortPacketsIgnored(t *testing.T) {
	ts := NewTimeService(nil)
	id := &mockIdentity{}
	pc := &packetCapture{}
	mc := NewMeshTimeConsensus(ts, id, pc.send)

	// These should not panic.
	mc.HandleTimeSyncRequest([]byte{0x14}, "test")
	mc.HandleTimeSyncRequest(make([]byte, 10), "test")
	mc.HandleTimeSyncResponse([]byte{0x15})
	mc.HandleTimeSyncResponse(make([]byte, 20))
}
