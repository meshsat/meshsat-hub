package aprsis

import (
	"context"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

type stubPlatform func(topic string) bool

func (f stubPlatform) IsPlatformTopic(_ context.Context, topic string) bool { return f(topic) }

// recordingBus keeps each subscription's handler and delivers a topic to
// every filter it matches ('+' only, which is all the subscriber uses).
type recordingBus struct{ handlers map[string]bus.MessageHandler }

func newRecordingBus() *recordingBus {
	return &recordingBus{handlers: map[string]bus.MessageHandler{}}
}

func (b *recordingBus) Connect() error                            { return nil }
func (b *recordingBus) Publish(string, byte, bool, []byte) error  { return nil }
func (b *recordingBus) PublishJSON(string, byte, bool, any) error { return nil }
func (b *recordingBus) IsConnected() bool                         { return true }
func (b *recordingBus) Disconnect()                               {}
func (b *recordingBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.handlers[topic] = h
	return nil
}
func (b *recordingBus) QueueSubscribe(topic string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(topic, q, h)
}

func (b *recordingBus) deliver(topic string, payload []byte) {
	for filter, h := range b.handlers {
		if filterMatches(filter, topic) {
			h(topic, payload)
		}
	}
}

func filterMatches(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	if len(f) != len(tp) {
		return false
	}
	for i := range f {
		if f[i] != "+" && f[i] != tp[i] {
			return false
		}
	}
	return true
}

// APRS-IS is public: a customer's satellite position must never be injected
// under the operator's callsign. The rate limiter records a device the moment
// it is let through, before the send, so lastSent shows exactly what passed
// the gate without needing a network connection.
func TestAPRSInjectsOnlyPlatformTraffic(t *testing.T) {
	const customer, customerMO, platform = "300234069999777", "300234069999778", "300234063904190"

	b := newRecordingBus()
	never := NewClient("127.0.0.1:1", "N0CALL", 10, "-1", "") // never connected: Send fails, harmlessly
	s := NewSubscriber(b, never, stubPlatform(func(topic string) bool {
		return !strings.Contains(topic, "t_cust")
	}), 60)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	pos := []byte(`{"lat":52.37,"lon":4.90,"source":"iridium"}`)
	mo := []byte(`{"iridium_latitude":52.37,"iridium_longitude":4.90}`)
	b.deliver("meshsat/t_cust/"+customer+"/position", pos)
	b.deliver("meshsat/t_cust/"+customerMO+"/mo/decoded", mo)
	b.deliver("meshsat/"+platform+"/position", pos)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range []string{customer, customerMO} {
		if _, passed := s.lastSent[id]; passed {
			t.Errorf("customer device %s reached the APRS-IS injector", id)
		}
	}
	if _, passed := s.lastSent[platform]; !passed {
		t.Error("the platform device did not reach the injector: the gate is dropping everything")
	}
}

func TestAPRSStartRefusesWithoutAPlatformChecker(t *testing.T) {
	b := newRecordingBus()
	s := NewSubscriber(b, NewClient("127.0.0.1:1", "N0CALL", 10, "-1", ""), nil, 60)
	if err := s.Start(); err == nil {
		t.Fatal("Start succeeded with no platform checker")
	}
	if len(b.handlers) != 0 {
		t.Errorf("%d subscriptions made before refusing", len(b.handlers))
	}
}
