package personchat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The Community Post (MESHSAT-1733): a kit whose modem is a "post" device
// sends texts from many people, each ending in a pickup code, and two people
// writing to the same number from one post must never share a conversation.

const postModem = modemA

func newPostHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.st.profiles[postModem] = store.DeviceProfilePost
	h.st.labels[postModem] = "Post Leiden"
	return h
}

func TestPostCodeIsTheLastBracketedToken(t *testing.T) {
	for in, want := range map[string]string{
		"Maria: water at the north gate [#K7QW2X]": "K7QW2X",
		"SOS trapped, second floor [#ABCDEF]  ":    "ABCDEF",
		"no code here":                             "",
		"[#K7QW2X] in front":                       "",
		"bad alphabet [#K7QW0X]":                   "",
		"short [#K7QW2]":                           "",
	} {
		if got := PostCode(in); got != want {
			t.Errorf("PostCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTwoPeopleAtOnePostWritingToOneNumberNeverCollide(t *testing.T) {
	h := newPostHarness(t)
	// The tenant is in contacts mode and bob is nobody's contact: a post
	// device still reaches him (the lane runs open for it alone).
	h.setMode(tenantA, ModeContacts)
	h.mo(tenantA, postModem, "p1", "@+31612345002 Maria: water at the north gate [#K7QW2X]")
	h.clock = h.clock.Add(time.Minute)
	h.mo(tenantA, postModem, "p2", "@+31612345002 Nikos: need fuel at the depot [#ABCDEF]")
	got := h.sms[tenantA].out
	if len(got) != 2 || got[0].to != bob || got[1].to != bob {
		t.Fatalf("sms = %+v", got)
	}
	if got[0].body != "Post Leiden: Maria: water at the north gate [#K7QW2X]" || got[1].body != "Post Leiden: Nikos: need fuel at the depot [#ABCDEF]" {
		t.Fatalf("the person must see the code and never a device tag in the head: %+v", got)
	}
	if n := len(h.mtsTo(postModem)); n != 0 {
		t.Fatalf("a post's send must not notify the kit: %d", n)
	}
	var convs []*store.PersonConversation
	for _, c := range h.st.convs {
		if c.TenantID == tenantA {
			convs = append(convs, c)
		}
	}
	if len(convs) != 2 || convs[0].ID == convs[1].ID {
		t.Fatalf("two people at the post must be two conversations: %+v", convs)
	}
	tags := map[string]bool{}
	for _, c := range convs {
		if c.DeviceKind != DeviceKindPost || c.DeviceID != postModem || c.Peer != bob || c.LastOutAt.IsZero() {
			t.Fatalf("conversation = %+v", c)
		}
		tags[c.Tag] = true
	}
	if !tags["K7QW2X"] || !tags["ABCDEF"] {
		t.Fatalf("the codes are the tags: %v", tags)
	}
	// The same person at the post writing again with the same code (a
	// double tap the kit keyed) lands in the same conversation, and makes
	// it the most recent one to bob.
	h.clock = h.clock.Add(time.Minute)
	h.mo(tenantA, postModem, "p3", "@+31612345002 Maria: water at the north gate [#K7QW2X]")
	n := 0
	for _, c := range h.st.convs {
		if c.TenantID == tenantA {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("a repeated code must not open a third conversation: %d", n)
	}

	// Replies: the code in front (the SMS reply people are asked for), the
	// code without "#", the code quoted mid-text, and no code at all (the
	// most recent post to this person, since both are the same kit).
	ctx := context.Background()
	for i, tc := range []struct{ body, want string }{
		{"#ABCDEF on my way", "[from +31612345002] #ABCDEF on my way"},
		{"k7qw2x: ok, sending water", "[from +31612345002] #K7QW2X ok, sending water"},
		{"got it, ref K7QW2X, tomorrow", "[from +31612345002] #K7QW2X got it, ref K7QW2X, tomorrow"},
		{"thanks all", "[from +31612345002] #K7QW2X thanks all"},
	} {
		if !h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM" + string(rune('a'+i)), Body: tc.body}) {
			t.Fatalf("%q was not taken", tc.body)
		}
		mts := h.mtsTo(postModem)
		if len(mts) != i+1 || mts[i] != tc.want {
			t.Fatalf("reply %q: mts = %q, want %q", tc.body, mts, tc.want)
		}
	}
	// Three sends to bob (p1, p2 and the repeat), and no hint SMS: every
	// open conversation with him is the post's.
	if len(h.sms[tenantA].out) != 3 {
		t.Fatalf("no hint SMS is owed when every open conversation is the post's: %+v", h.sms[tenantA].out)
	}
}

func TestAClosedPostCodeDeliversToNobodyAndAStrangersCodeIsWords(t *testing.T) {
	h := newPostHarness(t)
	h.mo(tenantA, postModem, "p1", "@+31612345002 old news [#K7QW2X]")
	h.clock = h.clock.Add(h.svc.opts.Window + time.Hour) // K7QW2X's window closes
	h.mo(tenantA, postModem, "p2", "@+31612345002 fresh [#ABCDEF]")
	ctx := context.Background()
	if !h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM1", Body: "#K7QW2X late answer"}) {
		t.Fatal("the lane must take a reply to a known code even when closed")
	}
	if n := len(h.mtsTo(postModem)); n != 0 {
		t.Fatalf("a closed code must deliver to nobody, not fall through to the open one: %q", h.mtsTo(postModem))
	}
	// A six-letter word that is nobody's code is just words, delivered to
	// the only open conversation whole.
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM2", Body: "THANKS a lot"})
	if mts := h.mtsTo(postModem); len(mts) != 1 || mts[0] != "[from +31612345002] #ABCDEF THANKS a lot" {
		t.Fatalf("mts = %q", mts)
	}
}

func TestAnOrdinaryDeviceKeepsItsFormBesideAPost(t *testing.T) {
	h := newPostHarness(t)
	h.setMode(tenantA, ModeContacts)
	// The ordinary device (Node Two) is still held to the contacts rule...
	h.mo(tenantA, modemA2, "o1", "@+31612345002 hello stranger [#K7QW2X]")
	if n := len(h.sms[tenantA].out); n != 0 {
		t.Fatalf("an ordinary device wrote to a stranger in contacts mode: %+v", h.sms[tenantA].out)
	}
	if mts := h.mtsTo(modemA2); len(mts) != 1 || !strings.Contains(mts[0], "not in this account's contacts") {
		t.Fatalf("the ordinary device must be told: %q", mts)
	}
	// ...and to a contact its conversation is the device's, not a code's,
	// and the reply comes back without one.
	h.mo(tenantA, modemA2, "o2", "@+31612345001 water [#K7QW2X]")
	c := h.st.conv(tenantA)
	if c == nil || c.DeviceKind != "satellite" || c.Tag == "K7QW2X" {
		t.Fatalf("an ordinary device's conversation took the post's form: %+v", c)
	}
	ctx := context.Background()
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "#K7QW2X on my way"})
	if mts := h.mtsTo(modemA2); len(mts) != 2 || mts[1] != "[from +31612345001] #K7QW2X on my way" {
		t.Fatalf("an ordinary device's reply carries the words as they came, no code head: %q", mts)
	}
}

func TestAPostAndAnotherDeviceOpenToOnePersonNeedTheTag(t *testing.T) {
	h := newPostHarness(t)
	h.setMode(tenantA, ModeOpen)
	h.mo(tenantA, postModem, "p1", "@+31612345002 from the post [#K7QW2X]")
	h.clock = h.clock.Add(time.Minute)
	h.mo(tenantA, modemA2, "o1", "@+31612345002 from node two")
	ctx := context.Background()
	// No code, no tag, two devices: undecided, the person gets the hint once.
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM1", Body: "which one?"})
	if len(h.mtsTo(postModem))+len(h.mtsTo(modemA2)) != 0 {
		t.Fatal("an untagged reply with two devices open must reach nobody")
	}
	out := h.sms[tenantA].out
	if len(out) != 3 || !strings.Contains(out[2].body, "Start your reply with") || !strings.Contains(out[2].body, "K7QW2X") {
		t.Fatalf("the hint must name the post's code among the tags: %+v", out)
	}
	// The post's code picks the post; the device's tag picks the device.
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM2", Body: "#K7QW2X for the post"})
	if mts := h.mtsTo(postModem); len(mts) != 1 || mts[0] != "[from +31612345002] #K7QW2X for the post" {
		t.Fatalf("post mts = %q", mts)
	}
	tag := deviceTag(tenantA, modemA2)
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: bob, MessageSID: "SM3", Body: tag + " for node two"})
	if mts := h.mtsTo(modemA2); len(mts) != 1 || mts[0] != "[from +31612345002] for node two" {
		t.Fatalf("node two mts = %q", mts)
	}
}

func TestAPostDeviceInOffModeSendsNothing(t *testing.T) {
	h := newPostHarness(t)
	h.setMode(tenantA, ModeOff)
	h.mo(tenantA, postModem, "p1", "@+31612345002 anything [#K7QW2X]")
	if len(h.sms[tenantA].out) != 0 || len(h.mtsTo(postModem)) != 0 {
		t.Fatal("the tenant's off switch covers a post device too")
	}
}
