package sos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// recordingBus keeps every publish so a test can see the SOS event.
type recordingBus struct {
	topics []string
}

func (r *recordingBus) Connect() error                                                { return nil }
func (r *recordingBus) Disconnect()                                                   {}
func (r *recordingBus) IsConnected() bool                                             { return true }
func (r *recordingBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (r *recordingBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (r *recordingBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	r.topics = append(r.topics, topic)
	return nil
}
func (r *recordingBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	return r.Publish(topic, qos, retained, data)
}

// TestSOSOverPlainTextSMSEscalates: an SMS carries no imei in its mo/decoded
// payload, so the detector takes the device from the topic, the sender's
// phone number, and still raises the SOS event (MESHSAT-1022 follow-up).
func TestSOSOverPlainTextSMSEscalates(t *testing.T) {
	st, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rb := &recordingBus{}
	d := NewDetector(rb, nil, st, nil, "")
	const phone = "+31653618463"
	payload := []byte(`{"id":"sms-in-SM1","channel":"sms","text":"SOS tent collapsed","body":"SOS tent collapsed"}`)

	d.handleMODecoded(hubmqtt.TopicMODecodedFor("default", phone), payload)

	if len(rb.topics) != 1 {
		t.Fatalf("published %v, want exactly one SOS event", rb.topics)
	}
	if got, want := rb.topics[0], hubmqtt.TopicSOSFor("default", phone); got != want {
		t.Errorf("SOS topic = %q, want %q", got, want)
	}
	if strings.Contains(rb.topics[0], "+") {
		t.Errorf("SOS topic %q carries a raw wildcard", rb.topics[0])
	}

	// No imei anywhere: nothing to attribute the SOS to, nothing published.
	rb.topics = nil
	d.handleMODecoded("meshsat/hub/status", []byte(`{"text":"SOS"}`))
	if len(rb.topics) != 0 {
		t.Errorf("SOS on a non-device topic published %v", rb.topics)
	}
}
