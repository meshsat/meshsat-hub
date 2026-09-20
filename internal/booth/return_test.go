package booth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type fakeReturn struct {
	byRef map[string]*store.BoothRelay
	open  []store.BoothRelay
}

func (f *fakeReturn) GetBoothRelayByRef(_ context.Context, _, ref string) (*store.BoothRelay, error) {
	return f.byRef[ref], nil
}
func (f *fakeReturn) OpenBoothRelaysFor(_ context.Context, _, _, _ string) ([]store.BoothRelay, error) {
	return f.open, nil
}

func relay(ref, sender, bridge string) store.BoothRelay {
	return store.BoothRelay{
		TenantID: tenant, Ref: ref, Sender: sender, Channel: ch,
		BridgeID: bridge, MeshDest: "!node1", ExpiresAt: time.Now().Add(time.Hour),
	}
}

const kitA = "bridge-kit-a"

func TestReplyWithRefRoutesToThatVisitor(t *testing.T) {
	r := relay("A7", "+31600000001", kitA)
	f := &fakeReturn{byRef: map[string]*store.BoothRelay{"A7": &r}}

	got, err := Resolve(context.Background(), f, tenant, kitA, "!node1", "#A7 got your message, thanks")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Relay == nil || got.Relay.Sender != "+31600000001" {
		t.Fatalf("routed to %+v", got.Relay)
	}
	if got.Body != "got your message, thanks" {
		t.Errorf("body = %q, the reference was not stripped", got.Body)
	}
}

func TestRefIsCaseInsensitive(t *testing.T) {
	r := relay("A7", "+31600000001", kitA)
	f := &fakeReturn{byRef: map[string]*store.BoothRelay{"A7": &r}}

	got, _ := Resolve(context.Background(), f, tenant, kitA, "!node1", "#a7 lowercase")
	if got.Relay == nil {
		t.Fatal("a lowercase reference did not resolve; a thumb keyboard will produce them")
	}
}

// With exactly one conversation open there is only one answer, so the T-Deck
// user should not have to retype a token.
func TestReplyWithoutRefRoutesWhenOnlyOneOpen(t *testing.T) {
	f := &fakeReturn{open: []store.BoothRelay{relay("A7", "+31600000001", kitA)}}

	got, err := Resolve(context.Background(), f, tenant, kitA, "!node1", "hello back")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Relay == nil || got.Relay.Sender != "+31600000001" {
		t.Fatalf("did not route the unambiguous case: %+v", got)
	}
}

// The case that made last-visitor-wins unacceptable.
func TestTwoOpenConversationsAreAmbiguousNotGuessed(t *testing.T) {
	f := &fakeReturn{open: []store.BoothRelay{
		relay("A7", "+31600000001", kitA),
		relay("B2", "+31600000002", kitA),
	}}

	got, err := Resolve(context.Background(), f, tenant, kitA, "!node1", "hello back")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Relay != nil {
		t.Fatalf("a reply was delivered to %q while two conversations were open", got.Relay.Sender)
	}
	if !got.Ambiguous || got.Candidates != 2 {
		t.Errorf("expected an ambiguous result with 2 candidates, got %+v", got)
	}
}

// A reference seen on someone else's screen must not route a reply across kits.
func TestRefFromAnotherKitDoesNotRoute(t *testing.T) {
	r := relay("A7", "+31600000001", "bridge-kit-b")
	f := &fakeReturn{byRef: map[string]*store.BoothRelay{"A7": &r}}

	got, _ := Resolve(context.Background(), f, tenant, kitA, "!node1", "#A7 wrong kit")
	if got.Relay != nil {
		t.Fatal("a reference belonging to another kit routed a reply")
	}
}

// An unknown reference must NOT quietly fall through to the single-open
// conversation: somebody typed a reference, and delivering their words to a
// different visitor is the worst available recovery.
func TestUnknownRefDoesNotFallBackToTheOpenConversation(t *testing.T) {
	f := &fakeReturn{
		byRef: map[string]*store.BoothRelay{},
		open:  []store.BoothRelay{relay("B2", "+31600000002", kitA)},
	}

	got, _ := Resolve(context.Background(), f, tenant, kitA, "!node1", "#ZZ who is this")
	if got.Relay != nil {
		t.Fatalf("an unknown reference was delivered to %q", got.Relay.Sender)
	}
}

func TestClosedConversationDoesNotRoute(t *testing.T) {
	closed := time.Now()
	r := relay("A7", "+31600000001", kitA)
	r.ClosedAt = &closed
	f := &fakeReturn{byRef: map[string]*store.BoothRelay{"A7": &r}}

	got, _ := Resolve(context.Background(), f, tenant, kitA, "!node1", "#A7 late reply")
	if got.Relay != nil {
		t.Fatal("a closed conversation accepted a reply")
	}
}

// A reference mid-sentence is the sender talking, not addressing. Treating it
// as a route would let a T-Deck user redirect by quoting a reference.
func TestRefMustBeAtTheStart(t *testing.T) {
	r := relay("A7", "+31600000001", kitA)
	f := &fakeReturn{
		byRef: map[string]*store.BoothRelay{"A7": &r},
		open:  []store.BoothRelay{relay("B2", "+31600000002", kitA)},
	}

	got, _ := Resolve(context.Background(), f, tenant, kitA, "!node1", "tell them #A7 was my seat number")
	if got.Relay != nil && got.Relay.Ref == "A7" {
		t.Fatal("a reference quoted mid-sentence was treated as a route")
	}
}

func TestNoOpenConversationsIsNotAnError(t *testing.T) {
	f := &fakeReturn{}
	got, err := Resolve(context.Background(), f, tenant, kitA, "!node1", "unsolicited")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Relay != nil || got.Ambiguous {
		t.Errorf("expected a clean no-match, got %+v", got)
	}
}

func TestAmbiguityPromptNamesNobody(t *testing.T) {
	// The guarantee is structural: AmbiguityPrompt takes a COUNT and nothing
	// else, so it has no visitor identity available to leak. This asserts the
	// observable half -- it says how many conversations are open, and carries no
	// phone number. The "#A7" in the text is a format example for the person on
	// the T-Deck, not anybody's reference.
	p := AmbiguityPrompt(2)
	if strings.Contains(p, "+31") || strings.Contains(p, "+") {
		t.Errorf("the ambiguity prompt contains a phone number: %s", p)
	}
	if !strings.Contains(p, "2") {
		t.Errorf("the prompt does not say how many are open: %s", p)
	}
}
