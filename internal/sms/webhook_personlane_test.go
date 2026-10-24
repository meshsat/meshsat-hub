package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/personchat"
)

type recordingLane struct {
	take bool
	got  []personchat.InboundSMS
	tids []string
}

func (l *recordingLane) TakeSMS(_ context.Context, tenantID string, in personchat.InboundSMS) bool {
	l.got = append(l.got, in)
	l.tids = append(l.tids, tenantID)
	return l.take
}

func decodedLane(t *testing.T, mb *mockBus) (string, bool) {
	t.Helper()
	for _, p := range mb.published {
		if strings.HasSuffix(p.topic, "/mo/decoded") {
			var m struct {
				Text string `json:"text"`
				Lane string `json:"lane"`
			}
			if err := json.Unmarshal(p.data, &m); err != nil {
				t.Fatal(err)
			}
			return m.Text, m.Lane == personchat.LaneMarker
		}
	}
	t.Fatal("a person's SMS must still be published on mo/decoded: SOS detection and the history read it there")
	return "", false
}

// A person's reply the "@" lane takes is still published, marked, so the SOS
// detector, the dead man's switch and the message history see it; only the
// routing engine leaves it alone. And a reply that merely LOOKS like base64
// ("Test", "Help") reaches the lane as typed, never as the binary pipeline's
// guess at a codec (MESHSAT-1685).
func TestAPersonsReplyGoesToTheLaneAndIsStillPublished(t *testing.T) {
	for _, body := range []string{"on my way", "Test", "Help"} {
		mb := &mockBus{}
		h := NewWebhookHandler(mb, "")
		h.SetInboundAuthToken(testAuthToken)
		lane := &recordingLane{take: true}
		h.SetPersonLane(lane)
		form := inboundForm()
		form.Set("Body", body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest(t, form))
		if w.Code != http.StatusOK {
			t.Fatalf("%q: HTTP %d", body, w.Code)
		}
		if len(lane.got) != 1 || lane.got[0].Body != body || lane.got[0].MessageSID != "SM999" ||
			lane.got[0].From != "+31612345678" || lane.got[0].Opened || lane.got[0].Sealed {
			t.Fatalf("%q: the lane got %+v", body, lane.got)
		}
		if lane.tids[0] != "default" {
			t.Fatalf("%q: tenant %q, want the webhook path's (default here)", body, lane.tids[0])
		}
		if _, marked := decodedLane(t, mb); !marked {
			t.Fatalf("%q: a taken reply must carry the lane marker", body)
		}
	}
}

func TestAnSMSTheLaneDoesNotTakeIsUnmarked(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	h.SetPersonLane(&recordingLane{take: false})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, inboundForm()))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	if _, marked := decodedLane(t, mb); marked {
		t.Fatal("an SMS the lane did not take was marked, so no route would see it")
	}
}
