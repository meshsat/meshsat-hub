package oob

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// ReplyTopic carries every OOB reply a replica receives to every other
// replica (MESHSAT-1164). The Hub runs two replicas behind round-robin: the
// pod that sent a command holds its waiter in an in-memory map, and the
// reply comes back through a webhook that lands on whichever pod the edge
// picks. Before this, a reply on the other pod was logged "rc ok" there and
// the caller got a timeout here. Same shape as webhook.ReloadTopic: announce
// on the bus, every replica delivers to its own map, the announcing replica
// skips its own message (it already delivered locally).
const ReplyTopic = "meshsat/hub/oob/reply"

// Bus is the slice of bus.MessageBus the service needs.
type Bus interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
	Subscribe(topic string, qos byte, handler bus.MessageHandler) error
}

type replyAnnouncement struct {
	Origin   string `json:"origin"` // the announcing replica, so it can ignore itself
	TenantID string `json:"tenant_id"`
	BridgeID string `json:"bridge_id"`
	Reply    Reply  `json:"reply"`
}

// SetBus subscribes the service to ReplyTopic and makes every reply it
// receives visible to the other replicas. Idempotent per service.
func (s *Service) SetBus(b Bus) error {
	s.mu.Lock()
	s.bus = b
	s.mu.Unlock()
	return b.Subscribe(ReplyTopic, 1, func(_ string, payload []byte) {
		var a replyAnnouncement
		if err := json.Unmarshal(payload, &a); err != nil {
			slog.Warn("oob: reply announcement unreadable", "error", err)
			return
		}
		if a.Origin == s.instance || a.BridgeID == "" {
			return
		}
		if s.deliver(a.BridgeID, a.Reply) {
			slog.Info("oob: reply matched a waiter on this replica via the bus", "bridge", a.BridgeID, "req_counter", a.Reply.Counter, "seq", a.Reply.Seq, "total", a.Reply.Total)
		}
	})
}

// deliver hands a reply to the local waiter for its request, if any.
func (s *Service) deliver(bridgeID string, r Reply) bool {
	s.mu.Lock()
	ch := s.pending[pendingKey(bridgeID, r.Counter)]
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- r:
	default:
	}
	return true
}

// announce tells the other replicas about a reply; a bus outage is logged,
// never an error to the webhook that carried the reply.
func (s *Service) announce(tenantID, bridgeID string, r Reply) {
	s.mu.Lock()
	b := s.bus
	s.mu.Unlock()
	if b == nil {
		return
	}
	payload, err := json.Marshal(replyAnnouncement{Origin: s.instance, TenantID: tenantID, BridgeID: bridgeID, Reply: r})
	if err != nil {
		return
	}
	if err := b.Publish(ReplyTopic, 1, false, payload); err != nil {
		slog.Warn("oob: reply announcement not published; a waiter on another replica will time out", "bridge", bridgeID, "error", err)
	}
}

func newInstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "replica"
	}
	return hex.EncodeToString(b)
}
