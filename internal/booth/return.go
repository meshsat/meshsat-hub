package booth

import (
	"context"
	"regexp"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The return leg: a reply came back off a kit's mesh, and it has to reach the
// visitor who started that conversation and nobody else.
//
// Getting this wrong is not a dropped message, it is one stranger's words
// arriving on another stranger's phone, in front of an audience.

// refPattern matches the token the mesh text carries, e.g. "#A7". Anchored to
// the start: a "#A7" appearing later in a sentence is the sender talking, not
// addressing, and treating it as a route would let the T-Deck user redirect a
// reply by quoting a reference they saw on someone else's screen.
var refPattern = regexp.MustCompile(`^\s*#([A-Za-z0-9]{2})\b[:,]?\s*`)

// Resolution is the outcome of trying to route one inbound mesh reply.
type Resolution struct {
	// Relay is the conversation this reply belongs to. Nil when Ambiguous or
	// when nothing matched.
	Relay *store.BoothRelay
	// Body is the reply with any reference token stripped, ready to send on.
	Body string
	// Ambiguous is true when the node has more than one open conversation and
	// the reply carried no reference. The caller must ask rather than guess.
	Ambiguous bool
	// Candidates is how many open conversations that node had, for the message
	// the caller sends back asking for a reference.
	Candidates int
}

// ReturnStore is the slice of the store the return leg needs.
type ReturnStore interface {
	GetBoothRelayByRef(ctx context.Context, tenantID, ref string) (*store.BoothRelay, error)
	OpenBoothRelaysFor(ctx context.Context, tenantID, bridgeID, meshDest string) ([]store.BoothRelay, error)
}

// Resolve decides which visitor an inbound mesh reply belongs to.
//
// Two ways in, in order of confidence:
//
//  1. The reply begins with the reference we put in the outbound text. That is
//     unambiguous and works no matter how many conversations the node has.
//  2. It carries no reference, and the node has exactly ONE open conversation.
//     Then there is only one answer it can be, and making the T-Deck user retype
//     a token they can see on screen would be a tax on the common case.
//
// Anything else is refused. In particular two open conversations and no
// reference is NOT resolved by recency: two visitors relaying to one node
// minutes apart is the normal case at a stand, and guessing there means
// answering the wrong person.
func Resolve(ctx context.Context, s ReturnStore, tenantID, bridgeID, meshDest, text string) (*Resolution, error) {
	if m := refPattern.FindStringSubmatch(text); m != nil {
		ref := strings.ToUpper(m[1])
		rel, err := s.GetBoothRelayByRef(ctx, tenantID, ref)
		if err != nil {
			return nil, err
		}
		if rel == nil || rel.ClosedAt != nil {
			// A reference we do not know, or one whose conversation has closed.
			// Deliberately not falling through to the single-conversation path:
			// somebody typed a reference, and quietly delivering their message
			// to a different visitor is the worst possible recovery.
			return &Resolution{Body: strings.TrimSpace(text)}, nil
		}
		// The reference must belong to the kit the reply actually came from.
		// Without this check a reference seen on another visitor's screen would
		// route a reply across kits.
		if rel.BridgeID != bridgeID {
			return &Resolution{Body: strings.TrimSpace(text)}, nil
		}
		return &Resolution{Relay: rel, Body: strings.TrimSpace(refPattern.ReplaceAllString(text, ""))}, nil
	}

	open, err := s.OpenBoothRelaysFor(ctx, tenantID, bridgeID, meshDest)
	if err != nil {
		return nil, err
	}
	switch len(open) {
	case 0:
		return &Resolution{Body: strings.TrimSpace(text)}, nil
	case 1:
		rel := open[0]
		return &Resolution{Relay: &rel, Body: strings.TrimSpace(text)}, nil
	default:
		return &Resolution{Ambiguous: true, Candidates: len(open), Body: strings.TrimSpace(text)}, nil
	}
}

// AmbiguityPrompt is what to put back on the mesh when a reply could belong to
// more than one visitor. It names no visitor and quotes no message: the person
// on the T-Deck is not entitled to know who else is talking to that node.
func AmbiguityPrompt(candidates int) string {
	return "There are " + itoa(candidates) + " open conversations on this node. " +
		"Start your reply with the reference, like #A7, so it reaches the right person."
}

// itoa avoids pulling strconv in for one small number.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
