// Package booth is the scripted visitor flow for the TTC stand (MESHSAT-1175).
//
// Two rules shape everything here.
//
// There is NO model in this path. The menu is fixed, the destinations are a
// fixed list, and what may go on the air is decided by the policy gate below and
// by nothing else. A stranger-reachable model with mesh injection rights is an
// open relay whose access control is a language model, and what it sends goes
// out over the owner's bearers, callsign and airtime. The agent that comes after
// TTC (MESHSAT-1176) sits on top of this and never inside it.
//
// The visitor never types a destination. They choose between two allowlisted
// meshes, and the choice is an INDEX into that list rather than an address, so
// there is no input through which the Hub can be talked into relaying elsewhere.
package booth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// States a visitor can be in. Persisted, because both Hub replicas serve the
// same visitor and the next message routinely lands on the other pod.
const (
	StateMenu          = "menu"
	StateAwaitingOptIn = "awaiting_optin"
	StateAwaitingKit   = "awaiting_kit"
	StateAwaitingText  = "awaiting_text"
)

// Option IDs. These are what a list row or button carries back to us, and they
// are the ONLY destination input the visitor has.
const (
	OptWhatIsMeshSat = "what_is_meshsat"
	OptSendMessage   = "send_message"
	OptOptInYes      = "optin_yes"
	OptOptInNo       = "optin_no"
	kitOptPrefix     = "kit:"
)

// Policy is the deterministic gate. Every field is a ceiling, and nothing in the
// conversation can raise one.
type Policy struct {
	// Destinations a visitor may choose between. Order is the menu order.
	Kits []Kit
	// MaxRunes caps a relayed message. Runes, not bytes: a cap in bytes cuts
	// multi-byte characters in half and puts mojibake on the mesh.
	MaxRunes int
	// PerSender relays allowed inside PerSenderWindow.
	PerSender       int
	PerSenderWindow time.Duration
	// Global relays allowed inside GlobalWindow, across every visitor.
	Global       int
	GlobalWindow time.Duration
	// RelayTTL is how long a conversation stays open for a reply.
	RelayTTL time.Duration
}

// Kit is one allowlisted destination.
type Kit struct {
	BridgeID string // the Hub's bridge id, e.g. nllei01parallax01
	Label    string // what the visitor sees
	MeshDest string // node/channel on that kit's mesh; "" means its default
}

// DefaultPolicy is the booth configuration agreed with the owner.
//
// Generous enough for a real visitor, tight enough that one person with a phone
// cannot flood the mesh or burn a roll of receipt paper.
func DefaultPolicy(kits []Kit) Policy {
	return Policy{
		Kits:            kits,
		MaxRunes:        160,
		PerSender:       3,
		PerSenderWindow: 10 * time.Minute,
		Global:          60,
		GlobalWindow:    time.Hour,
		// Short, because a kit serves one conversation at a time (MESHSAT-1178):
		// the TTL is the backstop for a reply that never comes, not the length of
		// an exchange. A conversation is normally closed the moment the reply is
		// delivered. Thirty minutes here would hold a kit hostage for half an
		// hour because one visitor wandered off.
		RelayTTL: 5 * time.Minute,
	}
}

// Reply is what the engine wants said back, as data rather than as WhatsApp
// wire format. The renderer turns it into an interactive list, buttons or text;
// keeping them apart is what makes the flow testable without Twilio.
type Reply struct {
	Text    string
	Options []Option // empty means a plain message
	// Relay is set when this turn actually put something on the mesh, so the
	// caller can send it and record the correlation.
	Relay *Relay
}

// Option is one tappable choice.
type Option struct {
	ID          string
	Label       string
	Description string
}

// Relay is a message cleared for transmission. It exists only when the policy
// gate passed; there is no other way to construct one from visitor input.
type Relay struct {
	Ref      string
	BridgeID string
	MeshDest string
	Body     string
	Expires  time.Time
}

// OnlineFunc reports whether a kit can currently take a message. A kit that is
// offline is refused out loud rather than queued: at a stand, a message that
// silently goes nowhere is worse than being told to try the other one.
type OnlineFunc func(ctx context.Context, bridgeID string) bool

// Store is the slice of the Hub's store this package needs.
//
// Narrow on purpose: the engine is the piece that decides what goes on the air,
// so it should be testable exhaustively without standing up a database, and it
// should not be able to reach anything it has no business touching.
type Store interface {
	GetBoothSession(ctx context.Context, tenantID, sender, channel string) (*store.BoothSession, error)
	SaveBoothSession(ctx context.Context, s *store.BoothSession) error
	CountBoothRelaysBySender(ctx context.Context, tenantID, sender string, since time.Time) (int, error)
	CountBoothRelays(ctx context.Context, tenantID string, since time.Time) (int, error)
	OpenBoothRelaysFor(ctx context.Context, tenantID, bridgeID, meshDest string) ([]store.BoothRelay, error)
}

// Engine runs the scripted flow.
type Engine struct {
	store  Store
	policy Policy
	online OnlineFunc
	now    func() time.Time
}

// New builds an engine. online may be nil, in which case every kit is treated
// as available -- useful in tests, never in production.
func New(s Store, p Policy, online OnlineFunc) *Engine {
	return &Engine{store: s, policy: p, online: online, now: time.Now}
}

var errNoKits = errors.New("booth: no destinations configured")

// Handle advances one visitor by one inbound message and returns what to say.
//
// choice is the option ID when the visitor tapped something, empty when they
// typed. text is what they typed. Both come straight off the inbound webhook.
func (e *Engine) Handle(ctx context.Context, tenantID, sender, channel, choice, text string) (*Reply, error) {
	if len(e.policy.Kits) == 0 {
		return nil, errNoKits
	}
	sess, err := e.store.GetBoothSession(ctx, tenantID, sender, channel)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		sess = &store.BoothSession{
			TenantID: tenantID, Sender: sender, Channel: channel, State: StateMenu,
		}
	}

	reply, next, err := e.step(ctx, sess, choice, text)
	if err != nil {
		return nil, err
	}
	sess.State = next
	if err := e.store.SaveBoothSession(ctx, sess); err != nil {
		return nil, err
	}
	return reply, nil
}

func (e *Engine) step(ctx context.Context, sess *store.BoothSession, choice, text string) (*Reply, string, error) {
	switch {
	// A tap on a menu row is honoured from any state. Somebody who wanders off
	// mid-flow and taps the menu again should get the menu, not a complaint.
	case choice == OptWhatIsMeshSat:
		return &Reply{Text: aboutText, Options: e.menuOptions()}, StateMenu, nil

	case choice == OptSendMessage:
		if sess.OptedInAt == nil {
			return &Reply{Text: optInText, Options: []Option{
				{ID: OptOptInYes, Label: "I agree"},
				{ID: OptOptInNo, Label: "No thanks"},
			}}, StateAwaitingOptIn, nil
		}
		return e.kitPrompt(), StateAwaitingKit, nil

	case choice == OptOptInNo:
		return &Reply{Text: "No problem. Nothing has been sent.", Options: e.menuOptions()}, StateMenu, nil

	case choice == OptOptInYes:
		// Consent is recorded the moment it is given, with a timestamp, and the
		// store never lets a later save clear it.
		now := e.now().UTC()
		sess.OptedInAt = &now
		return e.kitPrompt(), StateAwaitingKit, nil

	case strings.HasPrefix(choice, kitOptPrefix):
		kit, ok := e.kitByID(strings.TrimPrefix(choice, kitOptPrefix))
		if !ok {
			return e.kitPrompt(), StateAwaitingKit, nil
		}
		if e.online != nil && !e.online(ctx, kit.BridgeID) {
			return &Reply{
				Text:    fmt.Sprintf("%s is offline right now, so nothing would arrive. Try the other one.", kit.Label),
				Options: e.kitOptions(),
			}, StateAwaitingKit, nil
		}
		// The chosen kit is carried IN the persisted state, not in memory: the
		// visitor's next message routinely lands on the other replica, and a
		// destination held in a field on this process would be gone by then.
		return &Reply{Text: fmt.Sprintf("Type your message for %s. Up to %d characters.",
			kit.Label, e.policy.MaxRunes)}, StateAwaitingText + ":" + kit.BridgeID, nil

	// Typed text only means something when we asked for it. Anywhere else it is
	// a visitor talking to a menu, and the answer is the menu.
	case strings.HasPrefix(sess.State, StateAwaitingText) && text != "":
		return e.relay(ctx, sess, text)

	default:
		return &Reply{Text: welcomeText, Options: e.menuOptions()}, StateMenu, nil
	}
}

// relay is the gate. Everything that decides whether bytes go on the air is
// here, and none of it depends on interpreting what the visitor wrote.
func (e *Engine) relay(ctx context.Context, sess *store.BoothSession, text string) (*Reply, string, error) {
	// Consent, re-checked at the moment of transmission rather than trusted
	// from the menu path that led here.
	if sess.OptedInAt == nil {
		return &Reply{Text: optInText, Options: []Option{
			{ID: OptOptInYes, Label: "I agree"},
			{ID: OptOptInNo, Label: "No thanks"},
		}}, StateAwaitingOptIn, nil
	}

	// The destination was chosen from the allowlist earlier in the flow; it is
	// re-resolved here from the session rather than from anything in this
	// message, so a typed message can never redirect itself.
	kit, ok := e.kitForSession(sess)
	if !ok {
		return e.kitPrompt(), StateAwaitingKit, nil
	}

	body := strings.TrimSpace(text)
	if n := len([]rune(body)); n > e.policy.MaxRunes {
		return &Reply{Text: fmt.Sprintf("That is %d characters and the limit is %d. Shorten it and send again.",
			n, e.policy.MaxRunes)}, sess.State, nil
	}
	if body == "" {
		return &Reply{Text: "Nothing to send. Type a message."}, sess.State, nil
	}

	now := e.now()
	n, err := e.store.CountBoothRelaysBySender(ctx, sess.TenantID, sess.Sender, now.Add(-e.policy.PerSenderWindow))
	if err != nil {
		return nil, "", err
	}
	if n >= e.policy.PerSender {
		return &Reply{Text: "You have sent a few already. Give it a few minutes and come back.",
			Options: e.menuOptions()}, StateMenu, nil
	}
	total, err := e.store.CountBoothRelays(ctx, sess.TenantID, now.Add(-e.policy.GlobalWindow))
	if err != nil {
		return nil, "", err
	}
	if total >= e.policy.Global {
		return &Reply{Text: "The stand is busy and the mesh is at its limit for this hour. Try again shortly.",
			Options: e.menuOptions()}, StateMenu, nil
	}

	// One conversation per kit at a time.
	//
	// The T-Deck operator replies in plain text and will not retype a reference
	// (MESHSAT-1178). The return leg refuses to guess between two open
	// conversations, so without this a busy kit produces an ambiguity prompt on
	// the device mid-demo -- safe, but it reads as a malfunction to anyone
	// watching. Serialising means the ambiguity essentially never arises and the
	// operator never has to type anything. Two kits still means two concurrent
	// visitors.
	open, err := e.store.OpenBoothRelaysFor(ctx, sess.TenantID, kit.BridgeID, kit.MeshDest)
	if err != nil {
		return nil, "", err
	}
	for _, o := range open {
		// A visitor is never blocked by their own open conversation; that is
		// them continuing, not a collision.
		if o.Sender == sess.Sender {
			continue
		}
		return &Reply{
			Text:    fmt.Sprintf("%s is mid-conversation with someone else. Pick the other mesh, or try again in a moment.", kit.Label),
			Options: e.kitOptions(),
		}, StateAwaitingKit, nil
	}

	ref, err := newRef()
	if err != nil {
		return nil, "", err
	}
	rel := &Relay{
		Ref: ref, BridgeID: kit.BridgeID, MeshDest: kit.MeshDest,
		Body: body, Expires: now.Add(e.policy.RelayTTL),
	}
	return &Reply{
		Text:  fmt.Sprintf("Sent to %s, reference #%s. A reply will come back here.", kit.Label, ref),
		Relay: rel,
	}, StateMenu, nil
}

func (e *Engine) menuOptions() []Option {
	return []Option{
		{ID: OptWhatIsMeshSat, Label: "What is MeshSat", Description: "How this works"},
		{ID: OptSendMessage, Label: "Send a message", Description: "Put a message on the mesh"},
	}
}

func (e *Engine) kitOptions() []Option {
	out := make([]Option, 0, len(e.policy.Kits))
	for _, k := range e.policy.Kits {
		out = append(out, Option{ID: kitOptPrefix + k.BridgeID, Label: k.Label})
	}
	return out
}

func (e *Engine) kitPrompt() *Reply {
	return &Reply{Text: "Which mesh should it go to?", Options: e.kitOptions()}
}

func (e *Engine) kitByID(bridgeID string) (Kit, bool) {
	for _, k := range e.policy.Kits {
		if k.BridgeID == bridgeID {
			return k, true
		}
	}
	return Kit{}, false
}

// kitForSession recovers the destination chosen earlier. The chosen kit is held
// in the session state string as awaiting_text:<bridge_id>, so it survives the
// message landing on the other replica.
func (e *Engine) kitForSession(sess *store.BoothSession) (Kit, bool) {
	_, id, found := strings.Cut(sess.State, ":")
	if !found {
		return Kit{}, false
	}
	return e.kitByID(id)
}

// newRef is the short token the mesh text carries. Base32 over a crypto-random
// byte, uppercased: short enough to type on a T-Deck's thumb keyboard, and not
// sequential, so one visitor cannot guess another's reference and answer it.
func newRef() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I, O, 0, 1
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return string([]byte{alphabet[int(b[0])%len(alphabet)], alphabet[int(b[1])%len(alphabet)]}), nil
}

const (
	welcomeText = "Welcome to MeshSat. What would you like to do?"
	aboutText   = "MeshSat keeps people connected when the network is not: an open-source bridge between mesh radio and satellite. " +
		"The kits on this stand are real ones, and the message you send goes out over real radio."
	optInText = "Your message will be transmitted over radio and printed at this stand. " +
		"Your phone number is kept only to route the reply back to you, and is deleted after the event. Continue?"
)
