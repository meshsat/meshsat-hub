package booth

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Every booth message has to fit ONE SMS segment.
//
// This is not tidiness. Measured on the stand's own number: 24 of 24
// single-segment messages were delivered, and a 200-character consent prompt
// came back undelivered with carrier error 30008 (MESHSAT-1175). A visitor
// pressed 2 and nothing arrived — the Hub had answered, the carrier had
// dropped it, and nothing in our logs said so because Twilio accepted the send.
//
// The limit is 160 characters of GSM-7. A single character outside that
// alphabet — a curly apostrophe, an em dash, an accented letter — switches the
// whole message to UCS-2 and the limit collapses to 70, which is the trap this
// test really exists to catch.

// gsm7 is the GSM 03.38 basic set. Characters in the extension table cost two.
const gsm7 = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

const gsm7ext = "^{}\\[~]|€"

// smsSegments reports how many segments a body needs, and any characters that
// forced the message out of GSM-7.
func smsSegments(body string) (int, []rune) {
	var bad []rune
	n := 0
	for _, r := range body {
		switch {
		case strings.ContainsRune(gsm7, r):
			n++
		case strings.ContainsRune(gsm7ext, r):
			n += 2
		default:
			bad = append(bad, r)
			n++
		}
	}
	if len(bad) > 0 {
		// UCS-2: 70 per segment, 67 when concatenated.
		u := len([]rune(body))
		if u <= 70 {
			return 1, bad
		}
		return (u + 66) / 67, bad
	}
	if n <= 160 {
		return 1, nil
	}
	return (n + 152) / 153, nil
}

// TestEveryBoothMessageFitsOneSMS drives the flow through every branch that
// speaks to a visitor and measures what the SMS bearer would actually send --
// the text plus the numbered options, which is what deliver() concatenates.
func TestEveryBoothMessageFitsOneSMS(t *testing.T) {
	check := func(t *testing.T, label, body string) {
		t.Helper()
		segs, bad := smsSegments(body)
		if len(bad) > 0 {
			t.Errorf("%s: contains non-GSM-7 %q, which drops the limit to 70 characters:\n  %q",
				label, string(bad), body)
		}
		if segs > 1 {
			t.Errorf("%s: %d characters needs %d SMS segments; multi-segment is not reliably "+
				"delivered from this number:\n  %q", label, len(body), segs, body)
		}
	}

	// rendered is what the SMS bearer puts on the wire for one Reply.
	rendered := func(r *Reply) string {
		if len(r.Options) == 0 {
			return r.Text
		}
		return r.Text + "\n\n" + renderOptionsAsText(r.Options)
	}

	ctx := context.Background()

	// Walk the visitor-facing paths. Each entry is a label and the steps that
	// reach it, so a failure names the screen rather than a line number.
	paths := []struct {
		label string
		steps []struct{ choice, text string }
	}{
		{"welcome", []struct{ choice, text string }{{"", "hello"}}},
		{"about", []struct{ choice, text string }{{OptWhatIsMeshSat, ""}}},
		{"consent prompt", []struct{ choice, text string }{{OptSendMessage, ""}}},
		{"consent declined", []struct{ choice, text string }{{OptSendMessage, ""}, {OptOptInNo, ""}}},
		{"mesh picker", []struct{ choice, text string }{{OptSendMessage, ""}, {OptOptInYes, ""}}},
		{"awaiting text", []struct{ choice, text string }{
			{OptSendMessage, ""}, {OptOptInYes, ""}, {kitOptPrefix + "nllei01parallax01", ""}}},
		{"over the length cap", []struct{ choice, text string }{
			{OptSendMessage, ""}, {OptOptInYes, ""}, {kitOptPrefix + "nllei01parallax01", ""},
			{"", strings.Repeat("a", 400)}}},
		{"empty message", []struct{ choice, text string }{
			{OptSendMessage, ""}, {OptOptInYes, ""}, {kitOptPrefix + "nllei01parallax01", ""}, {"", "   "}}},
		{"relay confirmation", []struct{ choice, text string }{
			{OptSendMessage, ""}, {OptOptInYes, ""}, {kitOptPrefix + "nllei01parallax01", ""},
			{"", "hello from the stand"}}},
	}

	for _, p := range paths {
		t.Run(p.label, func(t *testing.T) {
			f := newFake()
			e := newEngine(f, nil)
			var last *Reply
			for _, st := range p.steps {
				r, err := e.Handle(ctx, tenant, who, "sms", st.choice, st.text)
				if err != nil {
					t.Fatalf("%s: %v", p.label, err)
				}
				last = r
			}
			check(t, p.label, rendered(last))
		})
	}

	// The two refusals a busy stand actually produces.
	t.Run("kit offline", func(t *testing.T) {
		f := newFake()
		e := newEngine(f, func(context.Context, string) bool { return false })
		_, _ = e.Handle(ctx, tenant, who, "sms", OptSendMessage, "")
		_, _ = e.Handle(ctx, tenant, who, "sms", OptOptInYes, "")
		r, _ := e.Handle(ctx, tenant, who, "sms", kitOptPrefix+"nllei01parallax01", "")
		check(t, "kit offline", rendered(r))
	})

	t.Run("per-sender quota", func(t *testing.T) {
		f := newFake()
		f.perSender = 99
		e := newEngine(f, nil)
		for _, c := range []string{OptSendMessage, OptOptInYes, kitOptPrefix + "nllei01parallax01"} {
			_, _ = e.Handle(ctx, tenant, who, "sms", c, "")
		}
		r, _ := e.Handle(ctx, tenant, who, "sms", "", "hello")
		check(t, "per-sender quota", rendered(r))
	})

	t.Run("global quota", func(t *testing.T) {
		f := newFake()
		f.global = 999
		e := newEngine(f, nil)
		for _, c := range []string{OptSendMessage, OptOptInYes, kitOptPrefix + "nllei01parallax01"} {
			_, _ = e.Handle(ctx, tenant, who, "sms", c, "")
		}
		r, _ := e.Handle(ctx, tenant, who, "sms", "", "hello")
		check(t, "global quota", rendered(r))
	})

	t.Run("kit already busy", func(t *testing.T) {
		f := newFake()
		f.open["nllei01parallax01"] = []store.BoothRelay{
			{Ref: "ZZ", Sender: "+31600009999", BridgeID: "nllei01parallax01"},
		}
		e := newEngine(f, nil)
		for _, c := range []string{OptSendMessage, OptOptInYes, kitOptPrefix + "nllei01parallax01"} {
			_, _ = e.Handle(ctx, tenant, who, "sms", c, "")
		}
		r, _ := e.Handle(ctx, tenant, who, "sms", "", "hello")
		check(t, "kit already busy", rendered(r))
	})

	// The ambiguity prompt goes onto the MESH, not to a phone, but it rides the
	// same 160-character discipline because the kit relays it by SMS.
	t.Run("ambiguity prompt", func(t *testing.T) {
		check(t, "ambiguity prompt", AmbiguityPrompt(2))
	})

	// The Service speaks to visitors too, on paths the state machine never
	// reaches: a sweep of a relay nobody answered, and the day's spend ceiling.
	// Same phones, same bearer, same limit -- and no menu option would have
	// walked the loop above into either one.
	t.Run("expiry notice", func(t *testing.T) {
		check(t, "expiry notice", fmt.Sprintf(expiredTextFmt, "A7"))
	})
	t.Run("budget reached", func(t *testing.T) {
		check(t, "budget reached", budgetReachedText)
	})

	// Mesh presence (MESHSAT-1181) adds text to the two busiest screens: the
	// picker grows a marker per kit, and a quiet kit gets its own prompt. The
	// picker is measured with EVERY kit marked, which is the longest it gets.
	t.Run("mesh picker, all live", func(t *testing.T) {
		f := newFake()
		e := newEngine(f, nil)
		e.SetMeshLive(func(context.Context, string, string) bool { return true })
		_, _ = e.Handle(ctx, tenant, who, "sms", OptSendMessage, "")
		r, _ := e.Handle(ctx, tenant, who, "sms", OptOptInYes, "")
		check(t, "mesh picker, all live", rendered(r))
	})

	t.Run("quiet kit prompt", func(t *testing.T) {
		f := newFake()
		e := newEngine(f, nil)
		e.SetMeshLive(func(context.Context, string, string) bool { return false })
		_, _ = e.Handle(ctx, tenant, who, "sms", OptSendMessage, "")
		_, _ = e.Handle(ctx, tenant, who, "sms", OptOptInYes, "")
		r, _ := e.Handle(ctx, tenant, who, "sms", kitOptPrefix+"nllei01parallax01", "")
		check(t, "quiet kit prompt", rendered(r))
	})
}

// A message that is exactly at the cap must still be one segment, and one
// character more must not be.
func TestSegmentCounterIsCalibrated(t *testing.T) {
	if n, _ := smsSegments(strings.Repeat("a", 160)); n != 1 {
		t.Errorf("160 GSM-7 characters counted as %d segments, want 1", n)
	}
	if n, _ := smsSegments(strings.Repeat("a", 161)); n != 2 {
		t.Errorf("161 GSM-7 characters counted as %d segments, want 2", n)
	}
	if _, bad := smsSegments("a curly apostrophe: ’"); len(bad) == 0 {
		t.Error("a curly apostrophe was accepted as GSM-7; it forces UCS-2 and a 70-character limit")
	}
	if _, bad := smsSegments("an em dash — here"); len(bad) == 0 {
		t.Error("an em dash was accepted as GSM-7")
	}
}
