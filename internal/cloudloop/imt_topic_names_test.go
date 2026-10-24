package cloudloop

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeIMTTopic(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"RAW", IMTTopicRaw},
		{"raw", IMTTopicRaw},
		{" Purple ", IMTTopicPurple},
		{"PINK", IMTTopicPink},
		{"RED", IMTTopicRed},
		{"ORANGE", IMTTopicOrange},
		{"YELLOW", IMTTopicYellow},
		{"ROCKREMOTE_CDM", IMTTopicRockRemote},
		{"IMT_TOPIC_RAW", IMTTopicRaw},
		{"imt_topic_yellow", IMTTopicYellow},
	} {
		got, err := NormalizeIMTTopic(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeIMTTopic(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"BLUE", "IMT_TOPIC_BLUE", "IMT_TOPIC_", "244", "RAW2"} {
		got, err := NormalizeIMTTopic(bad)
		if err == nil {
			t.Errorf("NormalizeIMTTopic(%q) = %q, want an error", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "RAW, PURPLE") {
			t.Errorf("NormalizeIMTTopic(%q) error %q does not list the accepted topics", bad, err)
		}
	}
}

func TestNormalizeRingStyle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"NORMAL", RingStyleNormal},
		{"urgent", RingStyleUrgent},
		{"EXTENDED", RingStyleExtended},
		{"RING_STYLE_URGENT", RingStyleUrgent},
	} {
		got, err := NormalizeRingStyle(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeRingStyle(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := NormalizeRingStyle("LOUD"); err == nil {
		t.Error("NormalizeRingStyle(LOUD) accepted an unknown style")
	}
}

// imtCall is one DoSendImtMessage call as Cloudloop saw it.
type imtCall struct {
	topic, ring string
	payload     []byte
}

type imtRecorder struct {
	mu    sync.Mutex
	calls []imtCall
}

func (c *imtRecorder) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		raw, err := base64.StdEncoding.DecodeString(q.Get("message"))
		if err != nil {
			t.Errorf("message does not decode: %v", err)
		}
		c.mu.Lock()
		c.calls = append(c.calls, imtCall{topic: q.Get("topic"), ring: q.Get("ringStyle"), payload: raw})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "mt-1", Status: "queued"})
	}))
}

func (c *imtRecorder) got() []imtCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]imtCall(nil), c.calls...)
}

func newIMTTestSender(t *testing.T, rec *imtRecorder) (*Sender, func()) {
	server := rec.server(t)
	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(imtResolver{thing: "THING-phone"})
	return s, server.Close
}

// The documented short name reaches Cloudloop as its full name. On 24 Oct
// 2026 "RAW" went out unchanged and arrived as IMT_TOPIC_UNKNOWN. [MESHSAT-1669]
func TestSendDirect_ShortTopicGoesOutFull(t *testing.T) {
	var rec imtRecorder
	s, done := newIMTTestSender(t, &rec)
	defer done()

	if _, err := s.SendDirect("300000000000001", MTSendRequest{Text: hubText, IMTTopic: "RAW", RingStyle: "urgent"}); err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	calls := rec.got()
	if len(calls) != 1 || calls[0].topic != IMTTopicRaw || calls[0].ring != RingStyleUrgent {
		t.Fatalf("Cloudloop saw %+v, want one call on %s with %s", calls, IMTTopicRaw, RingStyleUrgent)
	}
}

// An unknown topic is refused before anything is paid for.
func TestSendDirect_UnknownTopicSendsNothing(t *testing.T) {
	var rec imtRecorder
	s, done := newIMTTestSender(t, &rec)
	defer done()

	if _, err := s.SendDirect("300000000000001", MTSendRequest{Text: hubText, IMTTopic: "BLUE"}); err == nil {
		t.Fatal("SendDirect accepted an unknown topic")
	}
	if n := len(rec.got()); n != 0 {
		t.Fatalf("Cloudloop was called %d times for an unknown topic, want 0", n)
	}
}

// The satellite relay and federation hand over a verbatim payload with an
// empty topic (or Cloudloop's full name): it still goes out, as RAW, byte for
// byte.
func TestSendDirect_RelayedWireKeepsEmptyTopicAsRaw(t *testing.T) {
	wire := []byte{0x01, 0x8a, 0x00, 0xff, 0x42}
	for _, topic := range []string{"", IMTTopicRaw} {
		var rec imtRecorder
		s, done := newIMTTestSender(t, &rec)
		_, err := s.SendDirect("300000000000001", MTSendRequest{WireB64: base64.StdEncoding.EncodeToString(wire), IMTTopic: topic})
		done()
		if err != nil {
			t.Fatalf("topic %q: SendDirect: %v", topic, err)
		}
		calls := rec.got()
		if len(calls) != 1 || calls[0].topic != IMTTopicRaw || string(calls[0].payload) != string(wire) {
			t.Fatalf("topic %q: Cloudloop saw %+v, want the wire bytes once on %s", topic, calls, IMTTopicRaw)
		}
	}
}

// The MQTT mt/send path maps and refuses the same way.
func TestMTSend_TopicNames(t *testing.T) {
	var rec imtRecorder
	s, done := newIMTTestSender(t, &rec)
	defer done()

	body, _ := json.Marshal(MTSendRequest{Text: hubText, IMTTopic: "yellow"})
	s.handleMTSend("meshsat/300000000000001/mt/send", body)
	body, _ = json.Marshal(MTSendRequest{Text: hubText, IMTTopic: "BLUE"})
	s.handleMTSend("meshsat/300000000000001/mt/send", body)

	calls := rec.got()
	if len(calls) != 1 || calls[0].topic != IMTTopicYellow {
		t.Fatalf("Cloudloop saw %+v, want one call on %s and nothing for BLUE", calls, IMTTopicYellow)
	}
}
