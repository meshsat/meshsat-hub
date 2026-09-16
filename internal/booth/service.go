package booth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Service joins the engine to the two things it talks to: the visitor on
// WhatsApp, and the kit that puts the message on the air.
//
// The engine decides; this only carries. Nothing here may widen what the gate
// allowed -- if a turn produced no Relay, nothing reaches a kit.

// VisitorSender delivers to the visitor's phone.
type VisitorSender interface {
	SendText(ctx context.Context, to, body string) error
	SendContent(ctx context.Context, to, contentSid, contentVars string) error
}

// KitSender puts a message on a kit, over whichever bearer that kit uses.
type KitSender interface {
	SendToKit(ctx context.Context, tenantID, bridgeID, body string) error
}

// ServiceStore is everything the service needs beyond the engine's own slice.
type ServiceStore interface {
	Store
	ReturnStore
	CreateBoothRelay(ctx context.Context, r *store.BoothRelay) error
	CloseBoothRelay(ctx context.Context, tenantID, ref string) error
	ExpiredOpenBoothRelays(ctx context.Context, tenantID string, now time.Time) ([]store.BoothRelay, error)
	RecordBoothSend(ctx context.Context, tenantID, id, recipient, channel, kind string) error
	CountBoothSendsTo(ctx context.Context, tenantID, recipient string, since time.Time) (int, error)
	CountBoothSends(ctx context.Context, tenantID string, since time.Time) (int, error)
	// ClaimOnce records key atomically; exactly one caller across all replicas
	// gets true. See OnMeshReply for why the return leg needs it.
	ClaimOnce(ctx context.Context, key string) (bool, error)
}

// Templates are the Twilio Content SIDs for the interactive messages.
//
// Content resources are created once, out of band, and never submitted to Meta
// (MESHSAT-1175). They can only be delivered inside the 24h window a visitor
// opens by messaging us first, which is exactly the booth's shape.
type Templates struct {
	Menu  string // list picker: what_is_meshsat / send_message
	OptIn string // quick reply: optin_yes / optin_no
	Kits  string // list picker: kit:<bridge_id> per allowlisted kit
}

// Service is the booth flow, wired.
//
// visitors is per bearer, because the stand runs on both at once: WhatsApp when
// Meta allows it, SMS always. A conversation is answered on the bearer it
// started on, which the relay row records.
type Service struct {
	engine    *Engine
	store     ServiceStore
	visitors  map[string]VisitorSender
	kits      KitSender
	templates Templates
}

func NewService(e *Engine, s ServiceStore, k KitSender, t Templates) *Service {
	return &Service{engine: e, store: s, visitors: map[string]VisitorSender{}, kits: k, templates: t}
}

// RegisterVisitor attaches the sender for one bearer ("whatsapp", "sms").
func (s *Service) RegisterVisitor(channel string, v VisitorSender) {
	s.visitors[channel] = v
}

// visitorFor returns the sender for a bearer, or nil when that bearer is not
// enabled -- which is a real state: WhatsApp can be switched off entirely while
// SMS keeps the stand running.
func (s *Service) visitorFor(channel string) VisitorSender { return s.visitors[channel] }

// OnInbound handles one message from a visitor.
func (s *Service) OnInbound(ctx context.Context, tenantID, sender, channel, choice, text string) error {
	// The spend ceiling, checked before anything is generated.
	//
	// It counts MESSAGES, not relays: somebody who texts the keyword and walks
	// the menu without ever relaying still costs four messages, and the relay
	// quotas never see them. With a sub-USD-100 balance an afternoon of
	// curiosity could empty the account, so this is a hard stop rather than a
	// nudge. The visitor is told once and then it goes quiet -- the telling
	// itself costs a message, so it must not repeat.
	if ok, err := s.withinBudget(ctx, tenantID, sender, channel); err != nil {
		return err
	} else if !ok {
		return s.sayBudgetReached(ctx, tenantID, sender, channel)
	}

	reply, err := s.engine.Handle(ctx, tenantID, sender, channel, choice, text)
	if err != nil {
		return err
	}

	if reply.Relay != nil {
		// The correlation row is written BEFORE the message goes to the kit. A
		// mesh reply can come back in seconds, and a reply that arrives before
		// its own correlation exists is unroutable -- it would be dropped as
		// belonging to nobody.
		rel := &store.BoothRelay{
			TenantID: tenantID, Ref: reply.Relay.Ref, Sender: sender, Channel: channel,
			BridgeID: reply.Relay.BridgeID, MeshDest: reply.Relay.MeshDest,
			Body: reply.Relay.Body, ExpiresAt: reply.Relay.Expires,
		}
		if err := s.store.CreateBoothRelay(ctx, rel); err != nil {
			return err
		}
		// The token leads, bare and unbracketed: the Bridge rewrites brackets on
		// egress to a plaintext peer, so a bracketed token would not survive the
		// return leg (MESHSAT-1178).
		onAir := fmt.Sprintf("#%s %s", reply.Relay.Ref, reply.Relay.Body)
		if err := s.kits.SendToKit(ctx, tenantID, reply.Relay.BridgeID, onAir); err != nil {
			// Close it immediately: leaving the row open would hold a serialised
			// kit against a message that never went out, and the visitor would
			// be told it was sent.
			if cerr := s.store.CloseBoothRelay(ctx, tenantID, reply.Relay.Ref); cerr != nil {
				slog.Error("booth: could not close a relay that failed to send",
					"ref", reply.Relay.Ref, "error", cerr)
			}
			slog.Error("booth: relay to kit failed", "bridge", reply.Relay.BridgeID, "error", err)
			return s.sendOn(ctx, tenantID, channel, sender,
				"That did not get through to the kit. Nothing was sent -- try again in a moment.")
		}
		if err := s.store.RecordBoothSend(ctx, tenantID, xid.New().String(),
			reply.Relay.BridgeID, channel, "kit"); err != nil {
			slog.Warn("booth: could not record the kit send against the budget", "error", err)
		}
		slog.Info("booth: relayed", "ref", reply.Relay.Ref, "bridge", reply.Relay.BridgeID,
			"sender", sender, "bearer", channel, "len", len(reply.Relay.Body), "at", time.Now().UTC())
	}

	return s.deliver(ctx, tenantID, sender, channel, reply)
}

// OnMeshReply handles one message coming back off a kit's mesh.
//
// claimKey identifies the WIRE message and must be derived from it, not from a
// clock or a counter: both Hub replicas subscribe to mo/decoded and both receive
// every mesh reply, so without a once-only claim both resolve the same
// conversation and both send -- which is exactly what happened on the first
// working round trip, and the visitor got the reply twice.
//
// The claim is taken BEFORE the send rather than after. At-most-once is the
// right choice here: the alternative leaves a window where both replicas have
// resolved and neither has claimed, and a duplicate SMS costs money and reads as
// a fault to whoever is watching the stand.
func (s *Service) OnMeshReply(ctx context.Context, tenantID, bridgeID, meshDest, text, claimKey string) error {
	res, err := Resolve(ctx, s.store, tenantID, bridgeID, meshDest, text)
	if err != nil {
		return err
	}
	// Claim only once there is something to do, so ordinary mesh chatter does
	// not fill the claim table with keys for messages nobody acts on.
	if res.Relay != nil || res.Ambiguous {
		won, err := s.store.ClaimOnce(ctx, "booth-reply:"+tenantID+":"+claimKey)
		if err != nil {
			return err
		}
		if !won {
			// The other replica has it. Not an error, and not worth a warning:
			// this is the normal path for one of the two pods on every reply.
			slog.Debug("booth: mesh reply claimed by the other replica", "bridge", bridgeID)
			return nil
		}
	}

	switch {
	case res.Relay != nil:
		// Delivered on whichever bearer the visitor started on: the relay row
		// records it, so an SMS conversation is answered by SMS and a WhatsApp
		// one by WhatsApp, without the mesh side knowing either exists.
		if err := s.sendOn(ctx, tenantID, res.Relay.Channel, res.Relay.Sender, res.Body); err != nil {
			// Do NOT close on a send failure: the conversation is still the
			// right one, and closing would strand the reply with no way back.
			return err
		}
		slog.Info("booth: reply delivered", "ref", res.Relay.Ref, "bridge", bridgeID)
		// Closing here is what frees a serialised kit for the next visitor.
		return s.store.CloseBoothRelay(ctx, tenantID, res.Relay.Ref)

	case res.Ambiguous:
		// Should be rare: a kit takes one conversation at a time. It can still
		// happen if a conversation was opened before that rule, or by a second
		// path. Ask rather than guess.
		slog.Warn("booth: ambiguous mesh reply", "bridge", bridgeID, "candidates", res.Candidates)
		return s.kits.SendToKit(ctx, tenantID, bridgeID, AmbiguityPrompt(res.Candidates))

	default:
		// Reached only for a kit that IS on the stand allowlist -- the caller
		// drops everything else before this -- so it means a booth kit produced
		// mesh text that matched no conversation. Usually ordinary mesh chatter,
		// but it is also what a broken return leg looks like, and at Debug the
		// first one of those was invisible for an entire test round. Info.
		slog.Info("booth: mesh text matched no open conversation",
			"bridge", bridgeID, "mesh_dest", meshDest, "len", len(text))
		return nil
	}
}

// sendOn delivers plain text on one bearer. The visitor senders are registered
// per channel so the booth can serve both at once (MESHSAT-1175).
func (s *Service) sendOn(ctx context.Context, tenantID, channel, to, body string) error {
	return s.send(ctx, tenantID, channel, to, "visitor", body)
}

// deliver turns a Reply into a message. Options mean an interactive template;
// plain text is used when there is nothing to tap.
func (s *Service) deliver(ctx context.Context, tenantID, to, channel string, r *Reply) error {
	v := s.visitorFor(channel)
	if v == nil {
		return fmt.Errorf("booth: no sender for channel %q", channel)
	}
	rec := func() error {
		return s.store.RecordBoothSend(ctx, tenantID, xid.New().String(), to, channel, "visitor")
	}
	if len(r.Options) == 0 {
		if err := v.SendText(ctx, to, r.Text); err != nil {
			return err
		}
		return rec()
	}
	// Only WhatsApp has tappable rows. On SMS the same option set renders as a
	// numbered list and the visitor replies with a digit, which the engine
	// resolves against the options for their current state.
	sid := ""
	if channel == "whatsapp" {
		sid = s.templateFor(r.Options)
	}
	if sid == "" {
		if channel == "whatsapp" {
			// A WhatsApp option set with no template is a gap worth seeing; the
			// text fallback keeps the visitor moving rather than dead-ending.
			slog.Warn("booth: no content template for this option set", "options", optionIDs(r.Options))
		}
		if err := v.SendText(ctx, to, r.Text+"\n\n"+renderOptionsAsText(r.Options)); err != nil {
			return err
		}
		return rec()
	}
	vars, err := json.Marshal(map[string]string{"1": r.Text})
	if err != nil {
		return err
	}
	if err := v.SendContent(ctx, to, sid, string(vars)); err != nil {
		return err
	}
	return rec()
}

// templateFor picks the Content resource whose fixed items match this option
// set. The templates carry the items; only the body is variable.
func (s *Service) templateFor(opts []Option) string {
	if len(opts) == 0 {
		return ""
	}
	switch id := opts[0].ID; {
	case id == OptWhatIsMeshSat || id == OptSendMessage:
		return s.templates.Menu
	case id == OptOptInYes || id == OptOptInNo:
		return s.templates.OptIn
	case len(id) > len(kitOptPrefix) && id[:len(kitOptPrefix)] == kitOptPrefix:
		return s.templates.Kits
	default:
		return ""
	}
}

func optionIDs(opts []Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.ID)
	}
	return out
}

func renderOptionsAsText(opts []Option) string {
	var b []byte
	for i, o := range opts {
		b = append(b, []byte(fmt.Sprintf("%d. %s\n", i+1, o.Label))...)
	}
	return string(b)
}

// withinBudget reports whether another message may be sent.
func (s *Service) withinBudget(ctx context.Context, tenantID, recipient, channel string) (bool, error) {
	p := s.engine.policy
	now := s.engine.now()
	if p.PerRecipient > 0 {
		n, err := s.store.CountBoothSendsTo(ctx, tenantID, recipient, now.Add(-p.PerRecipientWindow))
		if err != nil {
			return false, err
		}
		if n >= p.PerRecipient {
			slog.Warn("booth: per-visitor message budget reached", "recipient", recipient, "sent", n)
			return false, nil
		}
	}
	if p.GlobalMessages > 0 {
		n, err := s.store.CountBoothSends(ctx, tenantID, now.Add(-p.GlobalWindowMsgs))
		if err != nil {
			return false, err
		}
		if n >= p.GlobalMessages {
			slog.Warn("booth: global message budget reached", "sent", n, "cap", p.GlobalMessages)
			return false, nil
		}
	}
	return true, nil
}

// sayBudgetReached tells the visitor once, then stays quiet. Claimed across
// replicas and across repeats, because the notice costs a message too.
func (s *Service) sayBudgetReached(ctx context.Context, tenantID, sender, channel string) error {
	key := "booth-budget:" + tenantID + ":" + sender + ":" + s.engine.now().UTC().Format("2006-01-02")
	won, err := s.store.ClaimOnce(ctx, key)
	if err != nil || !won {
		return err
	}
	return s.send(ctx, tenantID, channel, sender, "visitor",
		"That is all the messages the stand can send today. Come and say hello at the table instead.")
}

// send is the one place a message leaves the booth. Everything it sends is
// recorded, so the budget counts what was actually put on a bearer.
func (s *Service) send(ctx context.Context, tenantID, channel, to, kind, body string) error {
	v := s.visitorFor(channel)
	if v == nil {
		return fmt.Errorf("booth: no sender for channel %q", channel)
	}
	if err := v.SendText(ctx, to, body); err != nil {
		return err
	}
	return s.store.RecordBoothSend(ctx, tenantID, xid.New().String(), to, channel, kind)
}

// SweepExpired tells visitors whose reply never came, and frees the kit.
//
// Without this a visitor watches a silent phone: the kit is online, so the gate
// let the message through, but nothing on that mesh was listening. Silence is
// the worst answer a stand can give, and it is indistinguishable from a bug.
func (s *Service) SweepExpired(ctx context.Context, tenantID string) error {
	expired, err := s.store.ExpiredOpenBoothRelays(ctx, tenantID, s.engine.now())
	if err != nil {
		return err
	}
	for _, r := range expired {
		// Close first: the kit is freed whether or not the visitor can be told,
		// and closing is what stops this relay being swept again.
		if err := s.store.CloseBoothRelay(ctx, tenantID, r.Ref); err != nil {
			slog.Error("booth: could not close an expired relay", "ref", r.Ref, "error", err)
			continue
		}
		won, err := s.store.ClaimOnce(ctx, "booth-expired:"+tenantID+":"+r.Ref)
		if err != nil || !won {
			continue
		}
		ok, err := s.withinBudget(ctx, tenantID, r.Sender, r.Channel)
		if err != nil || !ok {
			continue
		}
		body := fmt.Sprintf("No answer came back for #%s. Nobody was listening on that mesh just now -- "+
			"try the other one, or come to the table.", r.Ref)
		if err := s.send(ctx, tenantID, r.Channel, r.Sender, "visitor", body); err != nil {
			slog.Error("booth: could not tell a visitor their relay expired", "ref", r.Ref, "error", err)
		}
		slog.Info("booth: relay expired unanswered", "ref", r.Ref, "bridge", r.BridgeID)
	}
	return nil
}
