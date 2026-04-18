package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/protocol"
)

func TestSendCommandTimeout(t *testing.T) {
	mb := newMockBus()
	cmdr := NewCommander(mb, nil)
	if err := cmdr.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmdr.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	cmd := protocol.Command{Cmd: "ping"}
	_, err := cmdr.SendCommand(ctx, "test-bridge", cmd)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !stringContains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func TestHandleResponseRouting(t *testing.T) {
	mb := newMockBus()
	cmdr := NewCommander(mb, nil)
	if err := cmdr.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmdr.Stop()

	var wg sync.WaitGroup
	wg.Add(1)

	var gotResp *protocol.CommandResponse
	var gotErr error

	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := protocol.Command{
			Cmd:       "ping",
			RequestID: "test-req-123",
		}
		gotResp, gotErr = cmdr.SendCommand(ctx, "bridge-01", cmd)
	}()

	// Give SendCommand time to register the pending channel and publish.
	time.Sleep(50 * time.Millisecond)

	// Simulate response arriving via MQTT using the existing mockBus.deliver.
	resp := &protocol.CommandResponse{
		Protocol:  protocol.ProtocolVersion,
		RequestID: "test-req-123",
		Cmd:       "ping",
		Status:    "ok",
		Result:    json.RawMessage(`{"uptime_sec":3600}`),
		Timestamp: time.Now().UTC(),
	}
	respPayload, _ := json.Marshal(resp)
	mb.deliver(protocol.TopicBridgeCmdResp("bridge-01"), respPayload)

	wg.Wait()

	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	if gotResp == nil {
		t.Fatal("expected response, got nil")
	}
	if gotResp.Status != "ok" {
		t.Fatalf("expected status ok, got %s", gotResp.Status)
	}
	if gotResp.RequestID != "test-req-123" {
		t.Fatalf("expected request_id test-req-123, got %s", gotResp.RequestID)
	}
}

func TestUnknownRequestIDNoPanic(t *testing.T) {
	mb := newMockBus()
	cmdr := NewCommander(mb, nil)
	if err := cmdr.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmdr.Stop()

	// Simulate a response for a request_id that nobody is waiting for.
	resp := &protocol.CommandResponse{
		Protocol:  protocol.ProtocolVersion,
		RequestID: "nonexistent-req",
		Cmd:       "ping",
		Status:    "ok",
		Timestamp: time.Now().UTC(),
	}
	respPayload, _ := json.Marshal(resp)
	mb.deliver(protocol.TopicBridgeCmdResp("unknown-bridge"), respPayload)
	// If we get here without panic, the test passes.
}

func TestDirectoryTrustAnchorRotateCommand(t *testing.T) {
	pub := []byte{0x30, 0x59, 0x01, 0x02, 0x03, 0x04}
	cmd := DirectoryTrustAnchorRotateCommand(pub, 7)

	if cmd.Cmd != "directory_trust_anchor_rotate" {
		t.Fatalf("cmd = %q, want directory_trust_anchor_rotate", cmd.Cmd)
	}
	if cmd.Timestamp.IsZero() {
		t.Fatal("timestamp not set")
	}

	var payload struct {
		PublicKey []byte `json:"public_key"`
		Algorithm string `json:"algorithm"`
		Version   int    `json:"version"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Algorithm != "ecdsa-p256" {
		t.Fatalf("algorithm = %q, want ecdsa-p256", payload.Algorithm)
	}
	if payload.Version != 7 {
		t.Fatalf("version = %d, want 7", payload.Version)
	}
	if string(payload.PublicKey) != string(pub) {
		t.Fatalf("public_key round-trip mismatch: got %x, want %x", payload.PublicKey, pub)
	}
}

// stringContains checks if s contains substr.
func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
