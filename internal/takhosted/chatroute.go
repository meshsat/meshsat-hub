package takhosted

import (
	"strings"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// Addressed events: a chat a dispatcher writes to one crew (MESHSAT-1706).
//
// An event that names its recipients (<marti><dest>) is for those recipients
// and nobody else. A TAK server honours that; the Hub must too, or a dispatcher's
// words to one handheld reach every kit of the tenant. So:
//
//   - From a client, an addressed event is never fanned out to the tenant's
//     other clients (it was, until this). A dest that is another client's crew
//     goes to that client alone; a dest that is one of the Hub's operators is
//     the operator's (takchat.ReplySink stores it on every replica, so the
//     leader only drops it here); any other dest is a TAK user, and the event
//     goes to the tenant's servers, which route it.
//   - From a server, an addressed event reached the Hub only because the Hub's
//     connection holds the crews' bindings (patch-relay-dms.py): it goes to the
//     client that owns each crew it names, and never to the broadcast.
//
// The routing to owners, the relay frame and the owners' write-behind are
// switched on by HUB_TAKCHAT_ENABLED (SetChat). Off, an addressed event from a
// server is delivered as before; the fan-out fix for clients holds either way.

const (
	cotRouted   = "routed"   // an addressed event handed to the client that owns its crew
	cotNoOwner  = "noowner"  // an addressed event for no crew the Hub knows (or a callsign two crews share)
	cotOperator = "operator" // a crew's answer to an operator: stored by takchat, not forwarded
)

// OperatorUIDPrefix is tak.OperatorUIDPrefix; init checks it lies under the
// Hub's own prefix.
const OperatorUIDPrefix = tak.OperatorUIDPrefix

func init() {
	if len(OperatorUIDPrefix) <= len(hubUIDPrefix) || OperatorUIDPrefix[:len(hubUIDPrefix)] != hubUIDPrefix {
		panic("takhosted: the operator uid prefix is not under the Hub's own")
	}
}

// contactWrites bounds what the owners' write-behind may have waiting.
const contactWrites = 256

// SetChat switches dispatcher-to-crew routing on or off (HUB_TAKCHAT_ENABLED).
func (f *Forwarder) SetChat(on bool) { f.chat.Store(on) }

func (f *Forwarder) chatEnabled() bool { return f.chat.Load() }

// observeOwner records that bridgeID exported a crew's position, and writes the
// owner behind to the store when that is due.
func (f *Forwarder) observeOwner(r *run, tenantID, bridgeID, uid, callsign string, now time.Time) {
	if x, ok := selfBridgeUID(uid); ok && x != bridgeID &&
		f.bridgeRegistered(r.ctx, tenantID, x) == regYes {
		// MESHSAT-<x> is bridge x itself; another bridge may not take it.
		return
	}
	owns, persist := r.owners.observe(tenantID, bridgeID, uid, callsign, now)
	if !owns || !f.chatEnabled() {
		return
	}
	if persist {
		select {
		case r.contacts <- store.TAKContact{TenantID: tenantID, UID: uid, Callsign: callsign, BridgeID: bridgeID, LastSeen: now}:
		default:
			f.log.Debug("takhosted: the TAK contact writer is behind, a sighting was not stored", "tenant", tenantID)
		}
	}
	f.sendRelay(r, tenantID, now)
}

// writeContacts stores owners behind the leader's memory until the run ends.
func (f *Forwarder) writeContacts(r *run) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case c := <-r.contacts:
			if f.store == nil {
				continue
			}
			if err := f.store.UpsertTAKContact(r.ctx, &c); err != nil && r.ctx.Err() == nil {
				f.log.Warn("takhosted: could not store a TAK contact", "tenant", c.TenantID, "error", err)
			}
		}
	}
}

// preloadOwners reads a tenant's owners back from the store once per tenure,
// so a new leader's first relay frame names the crews the last one knew.
func (f *Forwarder) preloadOwners(r *run, tenantID string) {
	if !f.chatEnabled() || f.store == nil || r.owners.isLoaded(tenantID) {
		return
	}
	contacts, err := f.store.ListTAKContacts(r.ctx, tenantID, time.Now().Add(-ownerFresh))
	if err != nil {
		f.log.Warn("takhosted: could not read a tenant's TAK contacts", "tenant", tenantID, "error", err)
		return
	}
	r.owners.load(tenantID, contacts)
}

// sendRelay queues the tenant's relay frame to each of its HOSTED servers when
// it is due. Never to a tenant's own server: a stock server would hand the frame
// to every client as an event.
func (f *Forwarder) sendRelay(r *run, tenantID string, now time.Time) {
	if !f.chatEnabled() || !r.owners.relayDue(tenantID, now) {
		return
	}
	frame := relayEvent(r.owners.relaySet(tenantID, now), now)
	for _, up := range f.upstreams(r.ctx, tenantID) {
		if up == nil || kindOf(up) != kindHosted {
			continue
		}
		// The priority queue: the bindings should be in place before the
		// positions queued behind them bring a dispatcher's chat.
		f.enqueue(r, tenantID, up, frame, true)
	}
}

// relayFrameFor is the frame a fresh connection to a hosted server sends after
// its hello, or nil.
func (f *Forwarder) relayFrameFor(r *run, tenantID string) []byte {
	if !f.chatEnabled() || r == nil {
		return nil
	}
	now := time.Now()
	crews := r.owners.relaySet(tenantID, now)
	if len(crews) == 0 {
		return nil
	}
	return relayEvent(crews, now)
}

// routeClientAddressed takes an addressed event a client exported. See the top
// of this file.
func (f *Forwarder) routeClientAddressed(r *run, tenantID, bridgeID string, line []byte, meta tak.Meta, dests []tak.Dest, now time.Time) {
	toServer := false
	routed := map[string]bool{}
	for _, d := range dests {
		if strings.HasPrefix(d.UID, OperatorUIDPrefix) {
			cotTotal.WithLabelValues(sourceClient, cotOperator).Inc()
			continue
		}
		if f.chatEnabled() {
			if owner, _, ok := r.owners.owner(tenantID, d, now); ok {
				if owner != bridgeID && !routed[owner] {
					routed[owner] = true
					f.deliverTo(tenantID, owner, line)
				}
				continue
			}
		}
		toServer = true
	}
	if len(routed) > 0 {
		cotTotal.WithLabelValues(sourceClient, cotRouted).Inc()
	}
	if !toServer {
		return
	}
	ups := f.upstreams(r.ctx, tenantID)
	if len(ups) == 0 {
		cotTotal.WithLabelValues(sourceClient, cotNowhere).Inc()
		return
	}
	r.sent.add(tenantID, meta.UID, meta.Time, now)
	out := append(append([]byte(nil), line...), '\n')
	for _, up := range ups {
		if up != nil {
			f.enqueue(r, tenantID, up, out, meta.Emergency)
		}
	}
	cotTotal.WithLabelValues(sourceClient, cotForwarded).Inc()
}

// routeUpstreamAddressed takes an addressed event read from a tenant's server to
// the client that owns each crew it names, and to nobody else.
func (f *Forwarder) routeUpstreamAddressed(r *run, tenantID string, line []byte, dests []tak.Dest, now time.Time) {
	owners := map[string]bool{}
	for _, d := range dests {
		if owner, _, ok := r.owners.owner(tenantID, d, now); ok {
			owners[owner] = true
		}
	}
	if len(owners) == 0 {
		cotTotal.WithLabelValues(sourceUpstream, cotNoOwner).Inc()
		return
	}
	for owner := range owners {
		f.deliverTo(tenantID, owner, line)
	}
	cotTotal.WithLabelValues(sourceUpstream, cotRouted).Inc()
}

// deliverTo publishes an addressed event to the one client that owns its crew:
// QoS 1, because a chat is a message and not a position, and never retained.
func (f *Forwarder) deliverTo(tenantID, bridgeID string, line []byte) {
	if f.bus == nil {
		return
	}
	if err := f.bus.Publish(hubmqtt.TopicTAKCotInFor(tenantID, bridgeID), 1, false, line); err != nil {
		f.log.Debug("takhosted: could not deliver an addressed event to its crew's client",
			"tenant", tenantID, "bridge", bridgeID, "error", err)
	}
}
