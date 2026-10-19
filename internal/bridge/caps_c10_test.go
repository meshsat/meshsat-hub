package bridge

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// capsBus is the mock bus that keeps what is published as JSON; with hold set, each publish
// waits for it to close, as a real publish waits for its PUBACK.
type capsBus struct {
	*mockBus
	mu   sync.Mutex
	pubs []capsPub
	hold chan struct{}
}

func (c *capsBus) capsOn(topic string) []capsPub {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capsPub
	for _, p := range c.pubs {
		if p.topic == topic {
			out = append(out, p)
		}
	}
	return out
}

type capsPub struct {
	topic    string
	retained bool
	body     map[string]any
}

func (c *capsBus) PublishJSON(topic string, _ byte, retained bool, v any) error {
	if c.hold != nil {
		<-c.hold
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	c.pubs = append(c.pubs, capsPub{topic, retained, m})
	c.mu.Unlock()
	return nil
}

func birthOf(id string, at time.Time) []byte {
	b, _ := json.Marshal(protocol.BridgeBirth{Protocol: protocol.ProtocolVersion, BridgeID: id, Version: "0.2.0", Mode: "direct",
		TenantID: "default", Timestamp: at})
	return b
}

// Every birth is answered on the bridge's retained hub/caps, saying sos_v2 when the Hub takes
// the C10 SOS frames and events; a stale (retained) birth too, since that is all a Hub that
// starts sees of a bridge that stayed connected; a Hub that was not told says nothing
// (HeMB-NG C10).
func TestABirthIsAnsweredWithTheHubsCaps(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   *bool
		stale bool
		want  []any // nil: no hub/caps at all
	}{
		{"sos_v2 on", ptr(true), false, []any{"sos_v2"}},
		{"sos_v2 off", ptr(false), false, []any{}},
		{"not told", nil, false, nil},
		{"a stale birth", ptr(true), true, []any{"sos_v2"}},
	} {
		mb := &capsBus{mockBus: newMockBus()}
		sub := NewSubscriber(mb, newMockStore(), nil)
		if c.set != nil {
			sub.SetSOSV2(*c.set)
		}
		if err := sub.Start(); err != nil {
			t.Fatal(err)
		}
		at := time.Now()
		if c.stale {
			at = at.Add(-time.Hour)
		}
		mb.deliver("meshsat/bridge/mule01/birth", birthOf("mule01", at))
		const topic = "meshsat/bridge/mule01/hub/caps"
		deadline := time.Now().Add(2 * time.Second)
		for c.want != nil && len(mb.capsOn(topic)) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if c.want == nil {
			time.Sleep(50 * time.Millisecond)
		}
		caps := mb.capsOn(topic)
		if c.want == nil {
			if len(caps) != 0 {
				t.Errorf("%s: published %+v", c.name, caps)
			}
			continue
		}
		if len(caps) != 1 || !caps[0].retained || caps[0].body["v"] != 1.0 {
			t.Fatalf("%s: %+v", c.name, caps)
		}
		got, _ := caps[0].body["caps"].([]any)
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("%s: caps %v, want %v", c.name, got, c.want)
		}
	}
}

func ptr(b bool) *bool { return &b }

// The birth handler never waits for the caps publish: it runs on the MQTT client's router, and
// a publish waits for a PUBACK the client cannot read while a handler holds it. A Hub that
// started with retained births waiting hung there and never opened its port (20 Oct 2026).
func TestTheBirthHandlerNeverWaitsForTheCapsPublish(t *testing.T) {
	mb := &capsBus{mockBus: newMockBus(), hold: make(chan struct{})}
	sub := NewSubscriber(mb, newMockStore(), nil)
	sub.SetSOSV2(true)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			mb.deliver("meshsat/bridge/mule01/birth", birthOf("mule01", time.Now().Add(-time.Hour)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(mb.hold)
		t.Fatal("the birth handler waited for the caps publish")
	}
	close(mb.hold)
	deadline := time.Now().Add(2 * time.Second)
	for len(mb.capsOn("meshsat/bridge/mule01/hub/caps")) < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(mb.capsOn("meshsat/bridge/mule01/hub/caps")); n != 5 {
		t.Fatalf("%d caps published, want 5", n)
	}
}

// A bridge never publishes the Hub's answers on its own subtree: sos/ack and hub/caps, in
// both topic shapes; it still publishes its sos/event and reads all three (C10).
func TestNATSPermissionsTheHubOwnsSOSAckAndCaps(t *testing.T) {
	for _, c := range []struct{ ns, root string }{{"meshsat", "meshsat"}, {"meshsat/t_x", "meshsat.t_x"}} {
		pub, sub := NATSPermissions(c.ns, "b1")
		deny := NATSPublishDeny(c.ns, "b1")
		own := c.root + ".bridge.b1."
		for subject, wantPub := range map[string]bool{
			own + "sos.event": true, own + "sos.ack": false, own + "sos.ack.x": false, own + "hub.caps": false, own + "hub.caps.x": false,
		} {
			if got := natsAllows(pub, subject) && !natsAllows(deny, subject); got != wantPub {
				t.Errorf("%s: publish %s = %v, want %v", c.ns, subject, got, wantPub)
			}
			if !natsAllows(sub, subject) {
				t.Errorf("%s: cannot subscribe to its own %s", c.ns, subject)
			}
		}
	}
}
