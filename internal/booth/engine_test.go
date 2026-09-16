package booth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// fakeStore is the four methods the engine uses, with counters a test can set.
type fakeStore struct {
	sess      map[string]*store.BoothSession
	perSender int
	global    int
	open      map[string][]store.BoothRelay // bridge id -> open conversations
	saveCalls int
	lastSaved *store.BoothSession
}

func newFake() *fakeStore {
	return &fakeStore{
		sess: map[string]*store.BoothSession{},
		open: map[string][]store.BoothRelay{},
	}
}

func (f *fakeStore) key(t, s, c string) string { return t + "|" + s + "|" + c }

func (f *fakeStore) GetBoothSession(_ context.Context, t, s, c string) (*store.BoothSession, error) {
	got, ok := f.sess[f.key(t, s, c)]
	if !ok {
		return nil, nil
	}
	cp := *got
	return &cp, nil
}

func (f *fakeStore) SaveBoothSession(_ context.Context, s *store.BoothSession) error {
	f.saveCalls++
	cp := *s
	// Mirror the real store's COALESCE: a save with no opt-in must not clear one.
	if prev, ok := f.sess[f.key(s.TenantID, s.Sender, s.Channel)]; ok && cp.OptedInAt == nil {
		cp.OptedInAt = prev.OptedInAt
	}
	f.sess[f.key(s.TenantID, s.Sender, s.Channel)] = &cp
	f.lastSaved = &cp
	return nil
}

func (f *fakeStore) CountBoothRelaysBySender(context.Context, string, string, time.Time) (int, error) {
	return f.perSender, nil
}
func (f *fakeStore) CountBoothRelays(context.Context, string, time.Time) (int, error) {
	return f.global, nil
}

func (f *fakeStore) OpenBoothRelaysFor(_ context.Context, _, bridgeID, _ string) ([]store.BoothRelay, error) {
	return f.open[bridgeID], nil
}

var testKits = []Kit{
	{BridgeID: "nllei01tesseract01", Label: "Tesseract"},
	{BridgeID: "nllei01parallax01", Label: "Parallax"},
}

func newEngine(f *fakeStore, online OnlineFunc) *Engine {
	return New(f, DefaultPolicy(testKits), online)
}

const (
	tenant = "t1"
	who    = "+31600000001"
	ch     = "whatsapp"
)

// walk drives the flow to the point where a message would be transmitted.
func walk(t *testing.T, e *Engine, f *fakeStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Handle(ctx, tenant, who, ch, OptSendMessage, ""); err != nil {
		t.Fatalf("menu: %v", err)
	}
	if _, err := e.Handle(ctx, tenant, who, ch, OptOptInYes, ""); err != nil {
		t.Fatalf("optin: %v", err)
	}
	if _, err := e.Handle(ctx, tenant, who, ch, kitOptPrefix+"nllei01parallax01", ""); err != nil {
		t.Fatalf("pick kit: %v", err)
	}
}

// Nothing goes on the air before consent is recorded. Tapping "send" without
// having opted in must produce the consent prompt, not a transmission.
func TestNoRelayWithoutOptIn(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	r, err := e.Handle(context.Background(), tenant, who, ch, OptSendMessage, "")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("a relay was produced before any opt-in")
	}
	// Assert on the OPTIONS, not the prose: the consent screen is identified by
	// offering a yes and a no, which is what makes it consent. Matching wording
	// made this test fail when the copy was shortened to fit one SMS segment.
	ids := optionIDs(r.Options)
	if len(ids) != 2 || ids[0] != OptOptInYes || ids[1] != OptOptInNo {
		t.Errorf("expected the consent prompt (yes/no), got options %v and text %q", ids, r.Text)
	}
}

// The gate is re-checked at the moment of transmission, not trusted from the
// path that led there. Reaching awaiting_text with the opt-in missing must
// still refuse.
func TestConsentIsRecheckedAtTransmission(t *testing.T) {
	f := newFake()
	f.sess[f.key(tenant, who, ch)] = &store.BoothSession{
		TenantID: tenant, Sender: who, Channel: ch,
		State: StateAwaitingText + ":nllei01parallax01", // in place, but never consented
	}
	e := newEngine(f, nil)
	r, err := e.Handle(context.Background(), tenant, who, ch, "", "hello")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("a message was transmitted for a visitor who never opted in")
	}
}

func TestHappyPathProducesARelay(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	walk(t, e, f)

	r, err := e.Handle(context.Background(), tenant, who, ch, "", "hello from the booth")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay == nil {
		t.Fatal("no relay produced on the happy path")
	}
	if r.Relay.BridgeID != "nllei01parallax01" {
		t.Errorf("relay went to %q, want the kit that was chosen", r.Relay.BridgeID)
	}
	if r.Relay.Body != "hello from the booth" {
		t.Errorf("body = %q", r.Relay.Body)
	}
	if r.Relay.Ref == "" {
		t.Error("relay has no ref, so no reply could ever be routed back")
	}
	if !strings.Contains(r.Text, r.Relay.Ref) {
		t.Error("the visitor was not told the reference")
	}
}

// The destination comes from the session, never from the message. This is the
// property that stops the relay being steerable by what a visitor types.
func TestTypedTextCannotRedirectTheRelay(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	walk(t, e, f) // chose parallax

	for _, attempt := range []string{
		"kit:nllei01tesseract01",
		"send to nllei01tesseract01",
		"@nllei01tesseract01 hello",
		"#A7 nllei01tesseract01",
	} {
		r, err := e.Handle(context.Background(), tenant, who, ch, "", attempt)
		if err != nil {
			t.Fatalf("handle %q: %v", attempt, err)
		}
		if r.Relay == nil {
			continue // refused for another reason, fine
		}
		if r.Relay.BridgeID != "nllei01parallax01" {
			t.Fatalf("text %q redirected the relay to %q", attempt, r.Relay.BridgeID)
		}
		// re-arm for the next attempt
		f.sess[f.key(tenant, who, ch)].State = StateAwaitingText + ":nllei01parallax01"
	}
}

func TestLengthCapIsEnforced(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	walk(t, e, f)

	long := strings.Repeat("a", 161)
	r, err := e.Handle(context.Background(), tenant, who, ch, "", long)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("an over-length message was transmitted")
	}
	if !strings.Contains(r.Text, "161") {
		t.Errorf("the visitor was not told the actual length: %q", r.Text)
	}
}

// Counted in runes, not bytes: 160 multi-byte characters is within the cap, and
// cutting on bytes would put half a character on the mesh.
func TestCapCountsRunesNotBytes(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	walk(t, e, f)

	r, err := e.Handle(context.Background(), tenant, who, ch, "", strings.Repeat("é", 160))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay == nil {
		t.Fatal("160 multi-byte characters were refused; the cap is counting bytes")
	}
}

func TestPerSenderQuota(t *testing.T) {
	f := newFake()
	f.perSender = 3 // already at the ceiling
	e := newEngine(f, nil)
	walk(t, e, f)

	r, _ := e.Handle(context.Background(), tenant, who, ch, "", "one more")
	if r.Relay != nil {
		t.Fatal("the per-sender quota did not stop a relay")
	}
}

func TestGlobalQuota(t *testing.T) {
	f := newFake()
	f.global = 60
	e := newEngine(f, nil)
	walk(t, e, f)

	r, _ := e.Handle(context.Background(), tenant, who, ch, "", "one more")
	if r.Relay != nil {
		t.Fatal("the global quota did not stop a relay")
	}
}

// An offline kit is refused out loud. Queueing would mean a visitor watching a
// stand where nothing happens and nobody can say why.
func TestOfflineKitIsRefusedNotQueued(t *testing.T) {
	f := newFake()
	e := newEngine(f, func(context.Context, string) bool { return false })

	ctx := context.Background()
	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	_, _ = e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
	r, err := e.Handle(ctx, tenant, who, ch, kitOptPrefix+"nllei01parallax01", "")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(r.Text, "offline") {
		t.Errorf("expected an offline refusal, got %q", r.Text)
	}
	if len(r.Options) == 0 {
		t.Error("the visitor was left with no way forward")
	}
}

// A destination that is not on the allowlist is not a destination.
func TestUnknownKitIsRejected(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	ctx := context.Background()
	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	_, _ = e.Handle(ctx, tenant, who, ch, OptOptInYes, "")

	r, err := e.Handle(ctx, tenant, who, ch, kitOptPrefix+"someone-elses-kit", "")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("an off-allowlist destination was accepted")
	}
}

// Typing at the menu is a visitor talking to a menu, not a transmission.
func TestTextOutsideAwaitingStateDoesNotRelay(t *testing.T) {
	f := newFake()
	e := newEngine(f, nil)
	r, err := e.Handle(context.Background(), tenant, who, ch, "", "just put this on the radio please")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("free text at the menu caused a transmission")
	}
}

// Refs must not be guessable in sequence, or one visitor could answer another's
// conversation by picking the next reference.
func TestRefsAreNotSequential(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		ref, err := newRef()
		if err != nil {
			t.Fatalf("newRef: %v", err)
		}
		if len(ref) != 2 {
			t.Fatalf("ref %q is not 2 characters", ref)
		}
		if strings.ContainsAny(ref, "IO01") {
			t.Errorf("ref %q contains a confusable character", ref)
		}
		seen[ref]++
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct refs in 200 draws; not random enough", len(seen))
	}
}

func TestNoKitsConfiguredIsAnError(t *testing.T) {
	e := New(newFake(), DefaultPolicy(nil), nil)
	if _, err := e.Handle(context.Background(), tenant, who, ch, OptSendMessage, ""); err == nil {
		t.Fatal("an engine with no destinations accepted a message")
	}
}

// One conversation per kit at a time (MESHSAT-1178). The T-Deck operator replies
// in plain text and will not retype a reference, so a busy kit would produce an
// ambiguity prompt on the device mid-demo. Serialising removes the situation.
func TestKitTakesOneConversationAtATime(t *testing.T) {
	f := newFake()
	f.open["nllei01parallax01"] = []store.BoothRelay{
		{Ref: "B2", Sender: "+31699999999", BridgeID: "nllei01parallax01"},
	}
	e := newEngine(f, nil)
	walk(t, e, f)

	r, err := e.Handle(context.Background(), tenant, who, ch, "", "hello")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay != nil {
		t.Fatal("a second visitor was let onto a kit that was mid-conversation")
	}
	if !strings.Contains(r.Text, "mid-conversation") {
		t.Errorf("the visitor was not told why: %q", r.Text)
	}
	if len(r.Options) == 0 {
		t.Error("the visitor was not offered the other mesh")
	}
}

// A visitor is not blocked by their OWN open conversation -- that is them
// continuing, not a collision.
func TestOwnOpenConversationDoesNotBlock(t *testing.T) {
	f := newFake()
	f.open["nllei01parallax01"] = []store.BoothRelay{
		{Ref: "A7", Sender: who, BridgeID: "nllei01parallax01"},
	}
	e := newEngine(f, nil)
	walk(t, e, f)

	r, err := e.Handle(context.Background(), tenant, who, ch, "", "following up")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.Relay == nil {
		t.Fatal("a visitor was blocked by their own open conversation")
	}
}

// The TTL is a backstop for a reply that never comes, not the length of an
// exchange. Long TTLs hold a serialised kit hostage.
func TestRelayTTLIsShort(t *testing.T) {
	if ttl := DefaultPolicy(testKits).RelayTTL; ttl > 10*time.Minute {
		t.Errorf("RelayTTL is %s; with one conversation per kit that blocks the kit for that long", ttl)
	}
}
