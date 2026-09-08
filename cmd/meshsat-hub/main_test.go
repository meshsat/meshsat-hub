package main

import (
	"os"
	"strings"
	"testing"
)

func TestMQTTClientIDUsesPodName(t *testing.T) {
	t.Setenv("POD_NAME", "hub-abc-xyz")
	if got := mqttClientID("meshsat-hub"); got != "meshsat-hub-hub-abc-xyz" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("POD_NAME", "")
	t.Setenv("HUB_MODE", "standalone")
	if got := mqttClientID("meshsat-hub"); got != "meshsat-hub" {
		t.Fatalf("standalone: got %q", got)
	}
}

func TestLeaderInstanceIDIsPerPod(t *testing.T) {
	t.Setenv("POD_NAME", "hub-abc-xyz")
	if got := leaderInstanceID("meshsat-hub"); got != "hub-abc-xyz" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("POD_NAME", "")
	got := leaderInstanceID("meshsat-hub")
	host, _ := os.Hostname()
	if !strings.HasPrefix(got, "meshsat-hub-") || (host != "" && !strings.Contains(got, host)) {
		t.Fatalf("without POD_NAME: got %q", got)
	}
}
