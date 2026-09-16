package booth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type sentText struct{ to, body string }
type sentContent struct{ to, sid, vars string }

type fakeVisitor struct {
	texts    []sentText
	contents []sentContent
	err      error
}

func (v *fakeVisitor) SendText(_ context.Context, to, body string) error {
	v.texts = append(v.texts, sentText{to, body})
	return v.err
}
func (v *fakeVisitor) SendContent(_ context.Context, to, sid, vars string) error {
	v.contents = append(v.contents, sentContent{to, sid, vars})
	return v.err
}

type fakeKits struct {
	sent []string
	err  error
}

func (k *fakeKits) SendToKit(_ context.Context, _, bridgeID, body string) error {
	if k.err != nil {
		return k.err
	}
	k.sent = append(k.sent, bridgeID+"|"+body)
	return nil
}

// fakeSvcStore is the engine's fake plus the relay rows.
type fakeSvcStore struct {
	*fakeStore
	relays map[string]*store.BoothRelay
	closed []string
}

func newSvcStore() *fakeSvcStore {
	return &fakeSvcStore{fakeStore: newFake(), relays: map[string]*store.BoothRelay{}}
}

func (f *fakeSvcStore) CreateBoothRelay(_ context.Context, r *store.BoothRelay) error {
	cp := *r
	f.relays[r.Ref] = &cp
	f.open[r.BridgeID] = append(f.open[r.BridgeID], cp)
	return nil
}
func (f *fakeSvcStore) CloseBoothRelay(_ context.Context, _, ref string) error {
	f.closed = append(f.closed, ref)
	if r, ok := f.relays[ref]; ok {
		now := time.Now()
		r.ClosedAt = &now
	}
	return nil
}
func (f *fakeSvcStore) GetBoothRelayByRef(_ context.Context, _, ref string) (*store.BoothRelay, error) {
	return f.relays[ref], nil
}

var testTemplates = Templates{Menu: "HXmenu", OptIn: "HXoptin", Kits: "HXkits"}

func newSvc(st *fakeSvcStore, v *fakeVisitor, k *fakeKits) *Service {
	e := New(st, DefaultPolicy(testKits), nil)
	return NewService(e, st, v, k, testTemplates)
}

func drive(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	for _, c := range []string{OptSendMessage, OptOptInYes, kitOptPrefix + "nllei01parallax01"} {
		if err := s.OnInbound(ctx, tenant, who, ch, c, ""); err != nil {
			t.Fatalf("drive %s: %v", c, err)
		}
	}
}

// The full forward leg: gate passes, correlation is written, the kit gets the
// message with the token leading and unbracketed.
func TestForwardLegRelaysWithABareToken(t *testing.T) {
	st, v, k := newSvcStore(), &fakeVisitor{}, &fakeKits{}
	s := newSvc(st, v, k)
	drive(t, s)

	if err := s.OnInbound(context.Background(), tenant, who, ch, "", "hello mesh"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(k.sent) != 1 {
		t.Fatalf("kit received %d messages, want 1", len(k.sent))
	}
	got := k.sent[0]
	if !strings.HasPrefix(got, "nllei01parallax01|#") {
		t.Fatalf("kit payload does not lead with a bare token: %q", got)
	}
	if strings.ContainsAny(got, "[]") {
		t.Errorf("the token is bracketed; the Bridge rewrites brackets on egress: %q", got)
	}
	if !strings.HasSuffix(got, " hello mesh") {
		t.Errorf("body not carried verbatim: %q", got)
	}
	if len(st.relays) != 1 {
		t.Errorf("correlation rows = %d, want 1", len(st.relays))
	}
}

// A turn that produced no Relay must never reach a kit.
func TestNothingReachesAKitWithoutARelay(t *testing.T) {
	st, v, k := newSvcStore(), &fakeVisitor{}, &fakeKits{}
	s := newSvc(st, v, k)

	// Tapping "send" only prompts for consent.
	if err := s.OnInbound(context.Background(), tenant, who, ch, OptSendMessage, ""); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if len(k.sent) != 0 {
		t.Fatalf("a kit was contacted without a relay: %v", k.sent)
	}
}

// If the kit cannot be reached, the visitor is told the truth and the row is
// closed -- otherwise a serialised kit stays blocked by a message that never
// went out, and the visitor believes it was sent.
func TestFailedKitSendClosesTheRelayAndSaysSo(t *testing.T) {
	st, v := newSvcStore(), &fakeVisitor{}
	k := &fakeKits{err: errors.New("modem offline")}
	s := newSvc(st, v, k)
	drive(t, s)

	if err := s.OnInbound(context.Background(), tenant, who, ch, "", "hello mesh"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(st.closed) != 1 {
		t.Fatalf("relay was left open after a failed send: closed=%v", st.closed)
	}
	last := v.texts[len(v.texts)-1].body
	if !strings.Contains(last, "Nothing was sent") {
		t.Errorf("the visitor was not told it failed: %q", last)
	}
}

// Options render as an interactive template; plain text has none.
func TestOptionsRenderAsContentAndTextDoesNot(t *testing.T) {
	st, v, k := newSvcStore(), &fakeVisitor{}, &fakeKits{}
	s := newSvc(st, v, k)

	if err := s.OnInbound(context.Background(), tenant, who, ch, OptSendMessage, ""); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if len(v.contents) == 0 {
		t.Fatal("an option set was sent as plain text instead of an interactive template")
	}
	if v.contents[len(v.contents)-1].sid != testTemplates.OptIn {
		t.Errorf("wrong template: %q", v.contents[len(v.contents)-1].sid)
	}

	drive(t, s)
	before := len(v.texts)
	_ = s.OnInbound(context.Background(), tenant, who, ch, "", "hello mesh")
	if len(v.texts) == before {
		t.Error("the confirmation, which has no options, was not sent as plain text")
	}
}

// The return leg delivers to the right visitor and frees the kit.
func TestReturnLegDeliversAndClosesTheConversation(t *testing.T) {
	st, v, k := newSvcStore(), &fakeVisitor{}, &fakeKits{}
	s := newSvc(st, v, k)
	drive(t, s)
	if err := s.OnInbound(context.Background(), tenant, who, ch, "", "hello mesh"); err != nil {
		t.Fatalf("send: %v", err)
	}
	var ref string
	for r := range st.relays {
		ref = r
	}

	before := len(v.texts)
	if err := s.OnMeshReply(context.Background(), tenant, "nllei01parallax01", "", "#"+ref+" got it"); err != nil {
		t.Fatalf("mesh reply: %v", err)
	}
	if len(v.texts) != before+1 {
		t.Fatalf("the reply was not delivered to the visitor")
	}
	got := v.texts[len(v.texts)-1]
	if got.to != who {
		t.Errorf("reply went to %q, want %q", got.to, who)
	}
	if got.body != "got it" {
		t.Errorf("reply body = %q, the token was not stripped", got.body)
	}
	if len(st.closed) == 0 {
		t.Error("the conversation was not closed, so the kit stays blocked")
	}
}

// Mesh traffic that belongs to no conversation is dropped, not forwarded to
// whoever happens to be around.
func TestUnrelatedMeshTextIsNotForwarded(t *testing.T) {
	st, v, k := newSvcStore(), &fakeVisitor{}, &fakeKits{}
	s := newSvc(st, v, k)

	if err := s.OnMeshReply(context.Background(), tenant, "nllei01parallax01", "", "random chatter"); err != nil {
		t.Fatalf("mesh reply: %v", err)
	}
	if len(v.texts) != 0 {
		t.Fatalf("unrelated mesh text was sent to a visitor: %v", v.texts)
	}
}
