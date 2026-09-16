package booth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

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
			return s.sendOn(ctx, channel, sender,
				"That did not get through to the kit. Nothing was sent -- try again in a moment.")
		}
		slog.Info("booth: relayed", "ref", reply.Relay.Ref, "bridge", reply.Relay.BridgeID,
			"sender", sender, "bearer", channel, "len", len(reply.Relay.Body), "at", time.Now().UTC())
	}

	return s.deliver(ctx, sender, channel, reply)
}

// OnMeshReply handles one message coming back off a kit's mesh.
func (s *Service) OnMeshReply(ctx context.Context, tenantID, bridgeID, meshDest, text string) error {
	res, err := Resolve(ctx, s.store, tenantID, bridgeID, meshDest, text)
	if err != nil {
		return err
	}

	switch {
	case res.Relay != nil:
		// Delivered on whichever bearer the visitor started on: the relay row
		// records it, so an SMS conversation is answered by SMS and a WhatsApp
		// one by WhatsApp, without the mesh side knowing either exists.
		if err := s.sendOn(ctx, res.Relay.Channel, res.Relay.Sender, res.Body); err != nil {
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
		// Not part of any conversation. Normal: the mesh carries other traffic.
		slog.Debug("booth: mesh message matched no conversation", "bridge", bridgeID)
		return nil
	}
}

// sendOn delivers plain text on one bearer. The visitor senders are registered
// per channel so the booth can serve both at once (MESHSAT-1175).
func (s *Service) sendOn(ctx context.Context, channel, to, body string) error {
	v := s.visitorFor(channel)
	if v == nil {
		return fmt.Errorf("booth: no sender for channel %q", channel)
	}
	return v.SendText(ctx, to, body)
}

// deliver turns a Reply into a message. Options mean an interactive template;
// plain text is used when there is nothing to tap.
func (s *Service) deliver(ctx context.Context, to, channel string, r *Reply) error {
	v := s.visitorFor(channel)
	if v == nil {
		return fmt.Errorf("booth: no sender for channel %q", channel)
	}
	if len(r.Options) == 0 {
		return v.SendText(ctx, to, r.Text)
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
		return v.SendText(ctx, to, r.Text+"\n\n"+renderOptionsAsText(r.Options))
	}
	vars, err := json.Marshal(map[string]string{"1": r.Text})
	if err != nil {
		return err
	}
	return v.SendContent(ctx, to, sid, string(vars))
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
