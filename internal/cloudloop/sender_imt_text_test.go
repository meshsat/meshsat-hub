package cloudloop

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/compress"
)

// cloudloopAsk records the payload each Cloudloop call carried, decoded:
// IMT takes it as base64, SBD as hex.
type cloudloopAsk struct {
	mu   sync.Mutex
	sent [][]byte
}

func (c *cloudloopAsk) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := r.URL.Query().Get("message")
		var raw []byte
		var err error
		if r.URL.Path == "/Data/DoSendImtMessage" {
			raw, err = base64.StdEncoding.DecodeString(msg)
		} else {
			raw, err = hex.DecodeString(msg)
		}
		if err != nil {
			t.Errorf("%s: message %q does not decode: %v", r.URL.Path, msg, err)
		}
		c.mu.Lock()
		c.sent = append(c.sent, raw)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "mt-1", Status: "queued"})
	}))
}

func (c *cloudloopAsk) only(t *testing.T) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) != 1 {
		t.Fatalf("Cloudloop was called %d times, want 1", len(c.sent))
	}
	return c.sent[0]
}

const hubText = "Test from the Hub, reply when you read this"

// A text to a 9704 is its own UTF-8 bytes: no version byte, no SMAZ2, even
// when the request asks for compression as the Messages page does. MeshSat
// Android read the compressed, versioned form as an encrypted chain and
// showed "cannot open". [MESHSAT-1675]
func TestSendDirect_IMTTextIsItsOwnBytes(t *testing.T) {
	var ask cloudloopAsk
	server := ask.server(t)
	defer server.Close()
	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(imtResolver{thing: "THING-phone"})

	res, err := s.SendDirect("300000000000001", MTSendRequest{Text: hubText, Compress: true})
	if err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	if got := ask.only(t); string(got) != hubText {
		t.Fatalf("a 9704 was sent % x, want the text's own bytes", got)
	}
	if res.WireBytes != len(hubText) {
		t.Errorf("wire_bytes = %d, want %d", res.WireBytes, len(hubText))
	}
}

// The same rule on the MQTT mt/send path.
func TestMTSend_IMTTextIsItsOwnBytes(t *testing.T) {
	var ask cloudloopAsk
	server := ask.server(t)
	defer server.Close()
	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(imtResolver{thing: "THING-phone"})

	body, _ := json.Marshal(MTSendRequest{Text: hubText, Compress: true})
	s.handleMTSend("meshsat/300000000000001/mt/send", body)
	if got := ask.only(t); string(got) != hubText {
		t.Fatalf("a 9704 was sent % x, want the text's own bytes", got)
	}
}

// An SBD modem keeps the version byte and the SMAZ2 it was asked for.
func TestSendDirect_SBDTextKeepsVersionByteAndSmaz2(t *testing.T) {
	var ask cloudloopAsk
	server := ask.server(t)
	defer server.Close()
	s := NewSender(NewClient(server.URL, "k"), &recordingBus{})
	s.SetDeviceResolver(staticResolver{thing: "THING-sbd"})

	if _, err := s.SendDirect("300434065000001", MTSendRequest{Text: hubText, Compress: true}); err != nil {
		t.Fatalf("SendDirect: %v", err)
	}
	want := append([]byte{0x01}, compress.Compress([]byte(hubText))...)
	if got := ask.only(t); string(got) != string(want) {
		t.Fatalf("an SBD modem was sent % x, want % x", got, want)
	}
}
