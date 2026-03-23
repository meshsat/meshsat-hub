package reticulum

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestPathRequestRoundtrip(t *testing.T) {
	var req PathRequest
	copy(req.DestHash[:], bytes.Repeat([]byte{0xAA}, TruncatedHashLen))
	copy(req.Tag[:], bytes.Repeat([]byte{0xBB}, TruncatedHashLen))

	data := MarshalPathRequest(&req)
	if len(data) != TruncatedHashLen*2 {
		t.Fatalf("expected %d bytes, got %d", TruncatedHashLen*2, len(data))
	}

	got, err := UnmarshalPathRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.DestHash != req.DestHash {
		t.Error("dest hash mismatch")
	}
	if got.Tag != req.Tag {
		t.Error("tag mismatch")
	}
}

func TestPathRequestTooShort(t *testing.T) {
	_, err := UnmarshalPathRequest(make([]byte, TruncatedHashLen-1))
	if err == nil {
		t.Fatal("expected error for short data")
	}
}

func TestPathResponseRoundtrip(t *testing.T) {
	resp := &PathResponse{
		Hops:          3,
		InterfaceType: "iridium",
		AnnounceData:  []byte("app-data-here"),
	}
	copy(resp.DestHash[:], bytes.Repeat([]byte{0xCC}, TruncatedHashLen))
	copy(resp.Tag[:], bytes.Repeat([]byte{0xDD}, TruncatedHashLen))

	data := MarshalPathResponse(resp)
	got, err := UnmarshalPathResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.DestHash != resp.DestHash {
		t.Error("dest hash mismatch")
	}
	if got.Tag != resp.Tag {
		t.Error("tag mismatch")
	}
	if got.Hops != 3 {
		t.Errorf("hops: got %d, want 3", got.Hops)
	}
	if got.InterfaceType != "iridium" {
		t.Errorf("iface: got %q, want iridium", got.InterfaceType)
	}
	if !bytes.Equal(got.AnnounceData, resp.AnnounceData) {
		t.Error("announce data mismatch")
	}
}

func TestPathResponseTooShort(t *testing.T) {
	_, err := UnmarshalPathResponse(make([]byte, 10))
	if err == nil {
		t.Fatal("expected error for short data")
	}
}

func TestPathResponseNoAnnounceData(t *testing.T) {
	resp := &PathResponse{
		Hops:          1,
		InterfaceType: "mqtt",
	}
	copy(resp.DestHash[:], bytes.Repeat([]byte{0x11}, TruncatedHashLen))
	copy(resp.Tag[:], bytes.Repeat([]byte{0x22}, TruncatedHashLen))

	data := MarshalPathResponse(resp)
	got, err := UnmarshalPathResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.InterfaceType != "mqtt" {
		t.Errorf("iface: got %q, want mqtt", got.InterfaceType)
	}
	if len(got.AnnounceData) != 0 {
		t.Errorf("expected nil announce data, got %d bytes", len(got.AnnounceData))
	}
}

func TestBuildPathRequestPacket(t *testing.T) {
	var destHash [TruncatedHashLen]byte
	rand.Read(destHash[:])

	req := &PathRequest{DestHash: destHash}
	rand.Read(req.Tag[:])

	pkt := BuildPathRequestPacket(destHash, req)
	hdr, err := UnmarshalHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.PacketType != PacketData {
		t.Errorf("packet type: got %d, want DATA", hdr.PacketType)
	}
	if hdr.DestType != DestPlain {
		t.Errorf("dest type: got %d, want PLAIN", hdr.DestType)
	}
	if hdr.Context != ContextRequest {
		t.Errorf("context: got %02x, want %02x", hdr.Context, ContextRequest)
	}
	if hdr.DestHash != destHash {
		t.Error("dest hash mismatch in header")
	}

	// Verify the data field is a valid path request.
	parsed, err := UnmarshalPathRequest(hdr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DestHash != destHash {
		t.Error("dest hash mismatch in request payload")
	}
}

func TestBuildPathResponsePacket(t *testing.T) {
	var destHash [TruncatedHashLen]byte
	rand.Read(destHash[:])

	resp := &PathResponse{
		DestHash:      destHash,
		Hops:          2,
		InterfaceType: "tor",
		AnnounceData:  []byte("test"),
	}
	rand.Read(resp.Tag[:])

	pkt := BuildPathResponsePacket(destHash, resp)
	hdr, err := UnmarshalHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Context != ContextPathResponse {
		t.Errorf("context: got %02x, want %02x", hdr.Context, ContextPathResponse)
	}

	parsed, err := UnmarshalPathResponse(hdr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hops != 2 {
		t.Errorf("hops: got %d, want 2", parsed.Hops)
	}
	if parsed.InterfaceType != "tor" {
		t.Errorf("iface: got %q, want tor", parsed.InterfaceType)
	}
}
