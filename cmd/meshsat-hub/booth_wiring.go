package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/booth"
	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Adapters joining the booth flow to the Hub's own senders and store
// (MESHSAT-1175). They live here rather than in internal/booth so that package
// stays free of the sms client and the full store, and remains testable on its
// own -- it is the piece that decides what goes on the air.

// whatsappVisitor delivers to the visitor's phone over the WhatsApp bearer.
type whatsappVisitor struct{ c *sms.Client }

func (w whatsappVisitor) SendText(ctx context.Context, to, body string) error {
	_, err := w.c.Send(ctx, to, body)
	return err
}

func (w whatsappVisitor) SendContent(ctx context.Context, to, sid, vars string) error {
	_, err := w.c.SendContent(ctx, to, sid, vars)
	return err
}

// smsKitSender puts a message on a kit over that kit's SIM.
//
// The kit's number is the OOB peer's Phone, which is where the Hub already
// keeps "the kit's SIM, E.164, for the SMS bearer". Using the same record means
// there is one answer to "how do we reach this kit by text" rather than two that
// can disagree.
type smsKitSender struct {
	store store.Store
	c     *sms.Client
}

func (k smsKitSender) SendToKit(ctx context.Context, tenantID, bridgeID, body string) error {
	peer, err := k.store.GetOOBPeer(ctx, tenantID, bridgeID)
	if err != nil {
		return fmt.Errorf("booth: look up kit %s: %w", bridgeID, err)
	}
	if peer == nil || peer.Phone == "" {
		return fmt.Errorf("booth: kit %s has no phone number on file", bridgeID)
	}
	_, err = k.c.Send(ctx, peer.Phone, body)
	return err
}

// boothInbound adapts the booth service to the webhook's hook.
//
// It always reports the message as taken. Everything arriving on the WhatsApp
// bearer while the booth is enabled is stand conversation, and letting an
// unrecognised message fall through to the message pipeline would persist a
// visitor's chatter into a tenant's history and run it through routing.
type boothInbound struct{ svc *booth.Service }

func (b boothInbound) HandleInbound(ctx context.Context, tenantID, sender, channel, choice, text string) (bool, error) {
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	return true, b.svc.OnInbound(ctx, tenantID, sender, channel, choice, text)
}

// parseBoothKits reads HUB_BOOTH_KITS, "<bridge_id>:<label>[:<mesh_dest>]",
// comma separated. The order is the order the visitor sees.
//
// This is the allowlist. A destination that is not in this string is not
// reachable from the stand, and nothing a visitor types can add one.
func parseBoothKits(raw string) ([]booth.Kit, error) {
	var out []booth.Kit
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Split(part, ":")
		if len(f) < 2 || f[0] == "" || f[1] == "" {
			return nil, fmt.Errorf("booth: %q is not <bridge_id>:<label>[:<mesh_dest>]", part)
		}
		k := booth.Kit{BridgeID: f[0], Label: f[1]}
		if len(f) > 2 {
			k.MeshDest = f[2]
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("booth: no kits configured")
	}
	return out, nil
}

// bridgeOnline reports whether a kit can take a message right now.
//
// A kit that is offline is refused out loud rather than queued: at a stand, a
// message that silently goes nowhere is worse than being told to try the other
// mesh.
func bridgeOnline(s store.Store) booth.OnlineFunc {
	return func(ctx context.Context, bridgeID string) bool {
		b, err := s.GetBridge(ctx, store.DefaultTenantID, bridgeID)
		if err != nil || b == nil {
			slog.Warn("booth: could not read bridge state", "bridge", bridgeID, "error", err)
			return false
		}
		return b.Online
	}
}

// startBoothMeshReplies routes a kit's inbound mesh text back to the visitor
// who started that conversation.
//
// The Bridge's event tap publishes inbound mesh text to meshsat/{device}/mo/decoded
// with bridge_id in the PAYLOAD (MESHSAT-1178), so the bridge is known without a
// device lookup -- which matters, because the Hub's Device record has no bridge
// field and mesh nodes are not registered devices in the satellite sense.
//
// Subscribed through the bus, never a raw paho client: paho keeps one callback
// per exact filter and the last Subscribe wins, which is how SOS detection and
// position storage silently starved for months (Critical Rule 13).
func startBoothMeshReplies(msgBus bus.MessageBus, svc *booth.Service, kits []booth.Kit) error {
	allowed := make(map[string]string, len(kits))
	for _, k := range kits {
		allowed[k.BridgeID] = k.MeshDest
	}

	handler := func(topic string, payload []byte) {
		var m struct {
			DeviceID string `json:"device_id"`
			BridgeID string `json:"bridge_id"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(payload, &m); err != nil {
			return
		}
		if m.Text == "" || m.BridgeID == "" {
			return
		}
		meshDest, ok := allowed[m.BridgeID]
		if !ok {
			// Not a stand kit. The mesh carries plenty that is not ours.
			return
		}
		// meshDest is the kit's CONFIGURED destination, not the node the reply
		// came from: a booth message goes to the kit's default channel, so the
		// conversation is keyed the same way and any node on that mesh may
		// answer it. With one conversation per kit that is unambiguous.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := svc.OnMeshReply(ctx, store.DefaultTenantID, m.BridgeID, meshDest, m.Text); err != nil {
			slog.Error("booth: mesh reply failed", "bridge", m.BridgeID, "device", m.DeviceID, "error", err)
		}
	}

	for _, filter := range hubmqtt.DualFilters(hubmqtt.TopicMODecoded("+")) {
		if err := msgBus.Subscribe(filter, 1, handler); err != nil {
			return fmt.Errorf("booth: subscribe %s: %w", filter, err)
		}
	}
	return nil
}
