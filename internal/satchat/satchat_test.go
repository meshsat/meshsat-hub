package satchat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Synthetic identities only: a test needs the shape, never a real device.
const (
	node     = "300000000000003" // the roaming satellite node
	kitA     = "bridge-kit-a"
	kitB     = "bridge-kit-b"
	kitAPhon = "+31600000001"
	kitBPhon = "+31600000002"
	stranger = "+31600000099"
)

type fakeStore struct {
	mu     sync.Mutex
	claims map[string]bool
}

func (f *fakeStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}

func (f *fakeStore) GetOOBPeer(_ context.Context, _, bridgeID string) (*store.OOBPeer, error) {
	switch bridgeID {
	case kitA:
		return &store.OOBPeer{BridgeID: kitA, Phone: kitAPhon}, nil
	case kitB:
		return &store.OOBPeer{BridgeID: kitB, Phone: kitBPhon}, nil
	}
	return nil, nil
}

type defaultTenant struct{}

func (defaultTenant) ForDeviceTopic(context.Context, string, string) string { return "default" }

type rig struct {
	svc  *Service
	sms  []string // "kit|body"
	mts  []string // "imei|text"
	now  time.Time
	stor *fakeStore
}

func (r *rig) SendToKit(_ context.Context, _, bridgeID, body string) error {
	r.sms = append(r.sms, bridgeID+"|"+body)
	return nil
}

// newRig shares one claim store between "replicas" when st is passed in.
func newRig(t *testing.T, st *fakeStore, devices ...string) *rig {
	t.Helper()
	if st == nil {
		st = &fakeStore{claims: map[string]bool{}}
	}
	if len(devices) == 0 {
		devices = []string{node}
	}
	r := &rig{now: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), stor: st}
	r.svc = New(nil, st, defaultTenant{}, devices,
		[]Kit{{BridgeID: kitA, Label: "Tesseract"}, {BridgeID: kitB, Label: "Parallax"}},
		r, func(_ context.Context, _, imei, text string) error {
			r.mts = append(r.mts, imei+"|"+text)
			return nil
		}, Options{})
	r.svc.now = func() time.Time { return r.now }
	return r
}

func satMO(id, text string) (string, []byte) {
	return "meshsat/" + node + "/mo/decoded",
		[]byte(fmt.Sprintf(`{"id":%q,"imei":%q,"channel":"iridium","text":%q}`, id, node, text))
}

func kitSMS(from, text string) (string, []byte) {
	return "meshsat/" + strings.ReplaceAll(from, "+", "%2B") + "/mo/decoded",
		[]byte(fmt.Sprintf(`{"id":"sms-%s-%d","channel":"sms","text":%q}`, from[len(from)-2:], len(text), text))
}

func kitMQTT(bridge, text string) (string, []byte) {
	return "meshsat/!0a0b0c0d/mo/decoded",
		[]byte(fmt.Sprintf(`{"device_id":"!0a0b0c0d","bridge_id":%q,"text":%q}`, bridge, text))
}

func TestStreetToStandTextsEveryKitWithTheToken(t *testing.T) {
	r := newRig(t, nil)
	tok := r.svc.byIMEI[node]
	r.svc.handle(satMO("mo-1", "hello from outside"))

	want := []string{kitA + "|*" + tok + " hello from outside", kitB + "|*" + tok + " hello from outside"}
	if fmt.Sprint(r.sms) != fmt.Sprint(want) {
		t.Fatalf("texts = %q\n want %q", r.sms, want)
	}

	// A provider redelivery of the same MO, and the other replica seeing it,
	// text nobody a second time.
	r.svc.handle(satMO("mo-1", "hello from outside"))
	other := newRig(t, r.stor)
	other.svc.handle(satMO("mo-1", "hello from outside"))
	if len(r.sms) != 2 || len(other.sms) != 0 {
		t.Fatalf("a redelivery texted the kits again: %q / %q", r.sms, other.sms)
	}
}

func TestOnlyReadableSatelliteTextFromALaneDeviceOpensTheLane(t *testing.T) {
	r := newRig(t, nil)
	for name, msg := range map[string][2]string{
		"an opaque payload (a kit's own encrypted satellite traffic)": {"meshsat/" + node + "/mo/decoded", `{"id":"a","imei":"` + node + `","channel":"iridium","text":"QUJD","opaque":true}`},
		"the same device, but forwarded over MQTT, not satellite":     {"meshsat/" + node + "/mo/decoded", `{"id":"b","channel":"mqtt","text":"hi"}`},
		"a satellite device that is not in the lane":                  {"meshsat/300000000000009/mo/decoded", `{"id":"c","channel":"iridium","text":"hi"}`},
		"empty text": {"meshsat/" + node + "/mo/decoded", `{"id":"d","channel":"iridium","text":"  "}`},
	} {
		r.svc.handle(msg[0], []byte(msg[1]))
		if len(r.sms) != 0 {
			t.Fatalf("%s texted the kits: %q", name, r.sms)
		}
	}
}

func TestStandToStreetGoesToTheModemTheTokenNames(t *testing.T) {
	r := newRig(t, nil)
	tok := r.svc.byIMEI[node]
	r.svc.handle(kitSMS(kitAPhon, "*"+strings.ToLower(tok)+"on my way")) // lower case, no space

	if len(r.mts) != 1 || r.mts[0] != node+"|Tesseract: on my way" {
		t.Fatalf("MTs = %q", r.mts)
	}
	if len(r.sms) != 0 {
		t.Fatalf("a reply was also texted to the kits: %q", r.sms)
	}
}

// The same reply arrives twice by design when a kit has internet: by SMS on its
// Hub lane and over MQTT from its mesh tap. One MT.
func TestAReplyThatArrivesOnBothPathsIsOneMT(t *testing.T) {
	r := newRig(t, nil)
	tok := r.svc.byIMEI[node]
	r.svc.handle(kitMQTT(kitA, "*"+tok+" on my way"))
	r.now = r.now.Add(40 * time.Second)
	r.svc.handle(kitSMS(kitAPhon, "*"+tok+"  On my way ")) // case and spacing differ
	// ... and a pair that straddles a ten-minute boundary is still one message.
	r.now = time.Date(2026, 9, 22, 10, 19, 58, 0, time.UTC)
	r.svc.handle(kitMQTT(kitB, "*"+tok+" second"))
	r.now = r.now.Add(5 * time.Second)
	r.svc.handle(kitSMS(kitBPhon, "*"+tok+" second"))

	if len(r.mts) != 2 {
		t.Fatalf("want 2 MTs (one per distinct reply), got %q", r.mts)
	}
	// The same words an hour later are a new message.
	r.now = r.now.Add(time.Hour)
	r.svc.handle(kitSMS(kitAPhon, "*"+tok+" on my way"))
	if len(r.mts) != 3 {
		t.Fatalf("the same words an hour later were dropped: %q", r.mts)
	}
}

// Anybody can text the Hub's number. Only a kit may spend satellite credit.
func TestAStarReplyFromAStrangerIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	tok := r.svc.byIMEI[node]
	r.svc.handle(kitSMS(stranger, "*"+tok+" send me money"))
	r.svc.handle(kitMQTT("some-other-bridge", "*"+tok+" send me money"))
	if len(r.mts) != 0 {
		t.Fatalf("a stranger reached the modem: %q", r.mts)
	}
}

// A kit that forwards its whole mesh to the Hub will send back the very text
// the Hub just put there. That is an echo, not a reply.
func TestTheHubsOwnMessageComingBackIsNotSentToTheModem(t *testing.T) {
	r := newRig(t, nil)
	tok := r.svc.byIMEI[node]
	r.svc.handle(satMO("mo-9", "hello from outside"))
	r.now = r.now.Add(20 * time.Second)
	r.svc.handle(kitSMS(kitAPhon, "*"+tok+" hello from outside"))
	if len(r.mts) != 0 {
		t.Fatalf("the Hub's own text was sent back up to the modem: %q", r.mts)
	}
	r.svc.handle(kitSMS(kitAPhon, "*"+tok+" got it"))
	if len(r.mts) != 1 {
		t.Fatalf("a real reply after an echo was lost: %q", r.mts)
	}
}

func TestAnUnknownTokenFallsBackOnlyWhenThereIsNoDoubt(t *testing.T) {
	one := newRig(t, nil)
	one.svc.handle(kitSMS(kitAPhon, "*Z9 mistyped"))
	if len(one.mts) != 1 || !strings.HasPrefix(one.mts[0], node+"|") {
		t.Fatalf("one device in the lane: a mistyped token should still reach it, got %q", one.mts)
	}
	two := newRig(t, nil, node, "300000000000004")
	unknown := "Z9"
	for _, known := range two.svc.byIMEI {
		if known == unknown {
			unknown = "Y8"
		}
	}
	two.svc.handle(kitSMS(kitAPhon, "*"+unknown+" mistyped"))
	if len(two.mts) != 0 {
		t.Fatalf("two devices in the lane: a guess would misdeliver, got %q", two.mts)
	}
}

func TestOtherLanesAreNotTouched(t *testing.T) {
	r := newRig(t, nil)
	for _, text := range []string{"#A7 visitor chat reply", "plain kit to kit text", "*", "* ", "** stars", "*hello everyone", "[+31600000002] relayed"} {
		r.svc.handle(kitSMS(kitAPhon, text))
		r.svc.handle(kitMQTT(kitA, text))
	}
	if len(r.mts) != 0 || len(r.sms) != 0 {
		t.Fatalf("the lane acted on traffic that is not its own: MTs %q, texts %q", r.mts, r.sms)
	}
}

func TestParseReply(t *testing.T) {
	for in, want := range map[string][2]string{
		"*K7 on my way":    {"K7", "on my way"},
		"*k7 on my way":    {"K7", "on my way"},
		"*K7on my way":     {"K7", "on my way"},
		" * K7: on my way": {"K7", "on my way"},
		"*K7 - ok":         {"K7", "ok"},
	} {
		tok, body, ok := ParseReply(in)
		if !ok || tok != want[0] || body != want[1] {
			t.Errorf("ParseReply(%q) = %q, %q, %v; want %q, %q", in, tok, body, ok, want[0], want[1])
		}
	}
	for _, in := range []string{"", "K7 no star", "*K7", "*K7   ", "*77 digits", "#K7 other lane"} {
		if _, _, ok := ParseReply(in); ok {
			t.Errorf("ParseReply(%q) accepted", in)
		}
	}
}

func TestTokensAreStableUniqueAndSMSSafe(t *testing.T) {
	list := []string{"300000000000001", "300000000000002", "300000000000003", "300000000000004", "300000000000005"}
	byTok, byIMEI := Tokens(list)
	rev := []string{list[4], list[2], list[0], list[3], list[1]}
	_, again := Tokens(rev)
	if len(byTok) != len(list) || len(byIMEI) != len(list) {
		t.Fatalf("not one token per device: %v", byIMEI)
	}
	for imei, tok := range byIMEI {
		if again[imei] != tok {
			t.Errorf("token for %s depends on list order: %s vs %s", imei, tok, again[imei])
		}
		if len(tok) != 2 || !strings.ContainsRune(tokenLetters, rune(tok[0])) || !strings.ContainsRune(tokenDigits, rune(tok[1])) {
			t.Errorf("token %q is not letter+digit from the safe alphabets", tok)
		}
	}
}

// From the Hub's number a multi-segment SMS is accepted and never delivered,
// and one character outside the GSM alphabet drops the limit from 160 to 70.
func TestFitSMSStaysInsideOneSegment(t *testing.T) {
	long := strings.Repeat("abcdefghij", 30)
	if got := FitSMS("*K7 ", long); utf8.RuneCountInString(got) != 160 || !strings.HasPrefix(got, "*K7 abc") {
		t.Errorf("plain GSM text: %d chars", utf8.RuneCountInString(got))
	}
	if got := FitSMS("*K7 ", "café "+long); utf8.RuneCountInString(got) != 160 {
		t.Errorf("é is in the GSM alphabet, want 160, got %d", utf8.RuneCountInString(got))
	}
	if got := FitSMS("*K7 ", "it’s "+long); utf8.RuneCountInString(got) != 70 {
		t.Errorf("a curly apostrophe forces UCS-2, want 70, got %d", utf8.RuneCountInString(got))
	}
	if got := FitSMS("*K7 ", strings.Repeat("[", 200)); utf8.RuneCountInString(got) != 82 { // 4 + 78 brackets at 2 septets = 160
		t.Errorf("extension-table characters cost two septets, got %d chars", utf8.RuneCountInString(got))
	}
}

func TestTheHourlyCeilingStopsARunawaySender(t *testing.T) {
	r := newRig(t, nil)
	r.svc.opts.MaxPerHour = 3
	for i := 0; i < 10; i++ {
		r.svc.handle(satMO(fmt.Sprintf("mo-%d", i), fmt.Sprintf("msg %d", i)))
	}
	if len(r.sms) != 6 { // 3 messages x 2 kits
		t.Fatalf("want 6 texts under a ceiling of 3 per hour, got %d", len(r.sms))
	}
}
