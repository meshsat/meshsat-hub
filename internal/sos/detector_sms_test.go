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
	const phone = "+31600000001"
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

// One Bridge SOS reaches the Hub by satellite, by SMS and from the Bridge
// over MQTT: its alert ids differ only in the bearer, and it raises one SOS
// event, not one per bearer. A different SOS (another start) still raises
// its own. [MESHSAT-1584]
func TestOneBridgeSOSByEveryBearerRaisesOneEvent(t *testing.T) {
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
	topic := hubmqtt.TopicMODecodedFor("default", "kit-01")
	for _, bearer := range []string{"imt", "sms", "sbd"} {
		d.handleMODecoded(topic, []byte(`{"id":"sos-`+bearer+`-kit-01-1792330000","imei":"kit-01","text":"SOS at the stand","sos":true}`))
	}
	if len(rb.topics) != 1 {
		t.Fatalf("published %d SOS events for one SOS; want 1", len(rb.topics))
	}
	d.handleMODecoded(topic, []byte(`{"id":"sos-imt-kit-01-1792339999","imei":"kit-01","text":"SOS again","sos":true}`))
	if len(rb.topics) != 2 {
		t.Fatalf("a second SOS (another start) raised %d events in all; want 2", len(rb.topics))
	}
	if sosClaimKey("sos-sms-kit-01-1792330000-cancelled") != "sos-kit-01-1792330000-cancelled" || sosClaimKey("sms-in-SM1") != "sms-in-SM1" {
		t.Fatal("claim key")
	}
}

// The accepted limit of the legacy claim (review 2026-10-18 18:26, F-D):
// two SOS runs from one Bridge that start in the same second share a claim,
// and another Bridge's SOS in that second does not. [MESHSAT-1584]
func TestTheLegacySOSClaimKeysOnBridgeAndSecond(t *testing.T) {
	if sosClaimKey("sos-imt-kit-01-1792330000") != sosClaimKey("sos-sms-kit-01-1792330000") {
		t.Fatal("one run by two bearers has two claims")
	}
	if sosClaimKey("sos-imt-kit-01-1792330000") == sosClaimKey("sos-imt-kit-02-1792330000") {
		t.Fatal("two Bridges in one second share a claim")
	}
	if sosClaimKey("sos-imt-kit-01-1792330000") == sosClaimKey("sos-imt-kit-01-1792330000-cancelled") {
		t.Fatal("an SOS and its cancellation share a claim")
	}
}
