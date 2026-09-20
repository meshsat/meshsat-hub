package sms

import (
	"bytes"
	"log/slog"
	"testing"
)

// The Twilio webhook republishes every inbound text on meshsat/hub/sms/inbound
// for the routing engine, and the Android subscriber's meshsat/+/sms/inbound
// filter matches it. It is not a device topic: it must be ignored without a
// warning (it warned "no device ID" on every SMS, MESHSAT-1164).
func TestHubSMSInboundTopicIsIgnoredWithoutAWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mb := newInboundMockBus()
	sub := NewInboundSubscriber(mb, nil, "default")
	_ = sub.Start()

	mb.fire("meshsat/hub/sms/inbound", []byte(`{"from":"+31600000002","body":"hello","timestamp":"2026-09-15T21:30:00Z"}`))
	if buf.Len() != 0 {
		t.Fatalf("the Hub's own topic produced a warning: %s", buf.String())
	}

	// A topic that is neither a device nor the Hub's still warns.
	mb.fire("meshsat", []byte(`{}`))
	if buf.Len() == 0 {
		t.Fatal("a malformed topic no longer warns; the guard is too wide")
	}
}
