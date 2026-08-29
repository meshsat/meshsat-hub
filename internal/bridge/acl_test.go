package bridge

import (
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func TestGeneratePasswordFile(t *testing.T) {
	bridges := []*store.Bridge{
		{BridgeID: "mule01", MQTTUsername: "mule01", MQTTPasswordHash: "$2a$10$hash1"},
		{BridgeID: "bananapi01", MQTTUsername: "bananapi01", MQTTPasswordHash: "$2a$10$hash2"},
		{BridgeID: "no-creds", MQTTUsername: "", MQTTPasswordHash: ""},
	}

	data := GeneratePasswordFile(bridges)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), string(data))
	}
	if lines[0] != "mule01:$2a$10$hash1" {
		t.Errorf("unexpected line 0: %s", lines[0])
	}
	if lines[1] != "bananapi01:$2a$10$hash2" {
		t.Errorf("unexpected line 1: %s", lines[1])
	}
}

func TestGeneratePasswordFile_Empty(t *testing.T) {
	data := GeneratePasswordFile(nil)
	if len(data) != 0 {
		t.Errorf("expected empty output, got: %q", string(data))
	}
}

func TestGenerateACLFile(t *testing.T) {
	bridges := []*store.Bridge{
		{BridgeID: "mule01", MQTTUsername: "mule01", MQTTPasswordHash: "$2a$10$hash1"},
		{BridgeID: "bananapi01", MQTTUsername: "bananapi01", MQTTPasswordHash: "$2a$10$hash2"},
	}

	data := GenerateACLFile(bridges)
	content := string(data)

	// Each bridge should have its own user block.
	if !strings.Contains(content, "user mule01") {
		t.Error("missing user mule01")
	}
	if !strings.Contains(content, "user bananapi01") {
		t.Error("missing user bananapi01")
	}

	// Each bridge should have readwrite on its own subtree.
	if !strings.Contains(content, "topic readwrite meshsat/bridge/mule01/#") {
		t.Error("missing readwrite topic for mule01")
	}
	if !strings.Contains(content, "topic readwrite meshsat/bridge/bananapi01/#") {
		t.Error("missing readwrite topic for bananapi01")
	}

	// Each bridge should have write access to device telemetry topics.
	if !strings.Contains(content, "topic write meshsat/+/position") {
		t.Error("missing write position topic")
	}
	if !strings.Contains(content, "topic write meshsat/+/sos") {
		t.Error("missing write sos topic")
	}
}

func TestGenerateACLFile_SkipsNoCreds(t *testing.T) {
	bridges := []*store.Bridge{
		{BridgeID: "no-user", MQTTUsername: ""},
	}
	data := GenerateACLFile(bridges)
	if len(data) != 0 {
		t.Errorf("expected empty output for bridges without credentials, got: %q", string(data))
	}
}

func TestGenerateACLFile_Empty(t *testing.T) {
	data := GenerateACLFile(nil)
	if len(data) != 0 {
		t.Errorf("expected empty output, got: %q", string(data))
	}
}
