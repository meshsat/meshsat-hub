package bridge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// capsBus is the mock bus that keeps what is published as JSON.
type capsBus struct {
	*mockBus
	pubs []capsPub
}

type capsPub struct {
	topic    string
	retained bool
	body     map[string]any
}

func (c *capsBus) PublishJSON(topic string, _ byte, retained bool, v any) error {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.pubs = append(c.pubs, capsPub{topic, retained, m})
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
		var caps []capsPub
		for _, p := range mb.pubs {
			if p.topic == "meshsat/bridge/mule01/hub/caps" {
				caps = append(caps, p)
			}
		}
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
