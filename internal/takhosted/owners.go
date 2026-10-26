package takhosted

import (
	"encoding/xml"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// Who owns a crew (MESHSAT-1706).
//
// A dispatcher who picks one crew in a TAK client and writes to it, or an
// operator who writes to it from the Hub's map, means one radio or one phone. The
// Hub must hand that chat to the one client that can reach it, and to no other:
// every kit hears some of the same nodes, and a chat delivered twice is two
// transmissions and two copies on a handheld. So the Hub remembers, per tenant,
// which bridge last exported each crew's position: that bridge owns the crew.
//
// # The rules
//
//   - Only a position event with a contact callsign makes an owner; a chat, a
//     marker or an SOS never does.
//   - Ownership is sticky. Another bridge exporting the same crew takes it over
//     only once the owner has been quiet for ownerSticky: two kits that both hear
//     a node would otherwise hand it back and forth with every position.
//   - A uid MESHSAT-<x>, where x is a bridge of the tenant, names that bridge
//     itself (a phone exports itself under it), and only bridge x may own it.
//   - A uid or callsign the tenant's TAK server has shown as one of ITS users is
//     never a crew: a client must not be able to take a dispatcher's own chats
//     by exporting a position under the dispatcher's name.
//   - A crew is reachable for ownerFresh after its last export.
//
// # Where it lives
//
// In the leader's memory, beside the forwarder's other per-tenure state, and
// written behind to the store (tak_contacts) so that the API on either replica
// and a new leader can read it. A new leader loads a tenant's owners when it
// first connects to that tenant's server.

const (
	ownerSticky     = 3 * time.Minute
	ownerFresh      = 24 * time.Hour
	ownerWriteEvery = 5 * time.Minute
	upstreamSeenFor = 24 * time.Hour
	// relayMax is the most crews one relay frame names (patch-relay-dms.py
	// keeps 1024 keys, a uid and a callsign each).
	relayMax = 512
	// relayEvery is how often the relay frame is sent again unchanged, for a
	// server that restarted and lost the bindings.
	relayEvery = 10 * time.Minute
)

type crewOwner struct {
	bridgeID string
	callsign string
	seen     time.Time
	written  time.Time
}

type owners struct {
	mu        sync.Mutex
	byTenant  map[string]map[string]*crewOwner // tenant -> crew uid -> owner
	upstream  map[string]map[string]time.Time  // tenant -> uid or callsign a TAK server showed
	changed   map[string]bool                  // tenants whose relay set changed
	lastRelay map[string]time.Time
	loaded    map[string]bool // tenants read back from the store this tenure
}

func newOwners() *owners {
	return &owners{
		byTenant:  map[string]map[string]*crewOwner{},
		upstream:  map[string]map[string]time.Time{},
		changed:   map[string]bool{},
		lastRelay: map[string]time.Time{},
		loaded:    map[string]bool{},
	}
}

// observe records that bridgeID exported crew uid under callsign. It answers
// whether the bridge owns the crew now, and whether the owner should be
// written to the store.
func (o *owners) observe(tenantID, bridgeID, uid, callsign string, now time.Time) (owns, persist bool) {
	if uid == "" || callsign == "" {
		return false, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if up := o.upstream[tenantID]; up != nil {
		if seenUpstream(up[uid], now) || seenUpstream(up["cs:"+callsign], now) {
			return false, false
		}
	}
	crews := o.byTenant[tenantID]
	if crews == nil {
		crews = map[string]*crewOwner{}
		o.byTenant[tenantID] = crews
	}
	cur := crews[uid]
	if cur != nil && cur.bridgeID != bridgeID && now.Sub(cur.seen) < ownerSticky {
		return false, false
	}
	if cur == nil {
		cur = &crewOwner{}
		crews[uid] = cur
	}
	if cur.bridgeID != bridgeID || cur.callsign != callsign {
		o.changed[tenantID] = true
		cur.written = time.Time{}
	}
	cur.bridgeID, cur.callsign, cur.seen = bridgeID, callsign, now
	if now.Sub(cur.written) >= ownerWriteEvery {
		cur.written = now
		return true, true
	}
	return true, false
}

// noteUpstream records a uid and callsign one of the tenant's TAK servers
// showed as its own user's position. Such a user stops being anybody's crew.
func (o *owners) noteUpstream(tenantID, uid, callsign string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	up := o.upstream[tenantID]
	if up == nil {
		up = map[string]time.Time{}
		o.upstream[tenantID] = up
	}
	if uid != "" {
		up[uid] = now
		if crews := o.byTenant[tenantID]; crews != nil && crews[uid] != nil {
			delete(crews, uid)
			o.changed[tenantID] = true
		}
	}
	if callsign != "" {
		up["cs:"+callsign] = now
	}
}

func seenUpstream(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) < upstreamSeenFor
}

// owner returns the bridge that owns a crew named by uid, or by a callsign
// exactly one fresh crew has, and the crew's uid.
func (o *owners) owner(tenantID string, d tak.Dest, now time.Time) (bridgeID, uid string, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	crews := o.byTenant[tenantID]
	if d.UID != "" {
		if c := crews[d.UID]; c != nil && now.Sub(c.seen) < ownerFresh {
			return c.bridgeID, d.UID, true
		}
		return "", "", false
	}
	if d.Callsign == "" {
		return "", "", false
	}
	if up := o.upstream[tenantID]; up != nil && seenUpstream(up["cs:"+d.Callsign], now) {
		return "", "", false
	}
	matches := 0
	for u, c := range crews {
		if c.callsign == d.Callsign && now.Sub(c.seen) < ownerFresh {
			bridgeID, uid = c.bridgeID, u
			matches++
		}
	}
	return bridgeID, uid, matches == 1
}

// relaySet is the crews a relay frame names: the fresh ones, newest first, at
// most relayMax.
func (o *owners) relaySet(tenantID string, now time.Time) []tak.ChatPeer {
	o.mu.Lock()
	defer o.mu.Unlock()
	type seen struct {
		peer tak.ChatPeer
		at   time.Time
	}
	var all []seen
	for uid, c := range o.byTenant[tenantID] {
		if now.Sub(c.seen) < ownerFresh {
			all = append(all, seen{tak.ChatPeer{UID: uid, Callsign: c.callsign}, c.seen})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.After(all[j].at)
		}
		return all[i].peer.UID < all[j].peer.UID
	})
	if len(all) > relayMax {
		all = all[:relayMax]
	}
	out := make([]tak.ChatPeer, len(all))
	for i, s := range all {
		out[i] = s.peer
	}
	return out
}

// relayDue reports whether a tenant's relay frame should go out now: its set
// changed, or relayEvery passed. It marks it sent.
func (o *owners) relayDue(tenantID string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.changed[tenantID] && now.Sub(o.lastRelay[tenantID]) < relayEvery {
		return false
	}
	o.changed[tenantID] = false
	o.lastRelay[tenantID] = now
	return true
}

// load takes a tenant's owners from the store, once per tenure, without
// replacing what this tenure has seen itself.
func (o *owners) load(tenantID string, contacts []store.TAKContact) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loaded[tenantID] = true
	crews := o.byTenant[tenantID]
	if crews == nil {
		crews = map[string]*crewOwner{}
		o.byTenant[tenantID] = crews
	}
	for _, c := range contacts {
		if _, have := crews[c.UID]; have || c.UID == "" || c.BridgeID == "" {
			continue
		}
		crews[c.UID] = &crewOwner{bridgeID: c.BridgeID, callsign: c.Callsign, seen: c.LastSeen, written: c.LastSeen}
		o.changed[tenantID] = true
	}
}

func (o *owners) isLoaded(tenantID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.loaded[tenantID]
}

func (o *owners) forget(tenantID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.byTenant, tenantID)
	delete(o.upstream, tenantID)
	delete(o.changed, tenantID)
	delete(o.lastRelay, tenantID)
	delete(o.loaded, tenantID)
}

func (o *owners) prune(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for tenantID, crews := range o.byTenant {
		for uid, c := range crews {
			if now.Sub(c.seen) >= ownerFresh {
				delete(crews, uid)
				o.changed[tenantID] = true
			}
		}
	}
	for _, up := range o.upstream {
		for k, at := range up {
			if !seenUpstream(at, now) {
				delete(up, k)
			}
		}
	}
}

// selfBridgeUID returns x for a uid "MESHSAT-<x>": the id a phone exports
// itself under, which may name a bridge.
func selfBridgeUID(uid string) (string, bool) {
	x, ok := strings.CutPrefix(uid, "MESHSAT-")
	return x, ok && x != ""
}

// relayEvent is the Hub-only event patch-relay-dms.py acts on: the crews this
// connection writes for.
func relayEvent(crews []tak.ChatPeer, now time.Time) []byte {
	const layout = "2006-01-02T15:04:05Z"
	ts := now.UTC().Format(layout)
	var b strings.Builder
	b.WriteString(`<event version="2.0" uid="` + hubUIDPrefix + `relay" type="` + relayType +
		`" how="h-g-i-g-o" time="` + ts + `" start="` + ts + `" stale="` + now.UTC().Add(time.Minute).Format(layout) +
		`"><point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail>`)
	for _, c := range crews {
		b.WriteString(`<relay uid="`)
		_ = xml.EscapeText(&b, []byte(c.UID))
		b.WriteString(`" callsign="`)
		_ = xml.EscapeText(&b, []byte(c.Callsign))
		b.WriteString(`"/>`)
	}
	b.WriteString("</detail></event>\n")
	return []byte(b.String())
}

// relayType is the relay event's CoT type.
const relayType = "t-x-meshsat-relay"
