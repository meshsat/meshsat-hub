package takhosted

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// CoT exported to the Hub by kits and apps (MESHSAT-1458).
//
// A Bridge, an Android phone and an iPhone each build Cursor-on-Target for what
// they know -- their own position, an SOS, a missed check-in, a chat message, the
// mesh nodes they relay for -- and can publish it to the Hub on the MQTT session
// they already hold. Until this file nothing listened: the Hub forwarded only the
// positions it built itself. This is the listener. An exported event is checked,
// re-written, and sent to every TAK server the tenant has, through the same links
// positions use.
//
// # Whose event it is
//
// The tenant is the one the TOPIC names, and the bridge named beside it must be
// registered to that tenant. Both halves matter.
//
// The topic, because the broker enforces it: a bridge credential may publish
// only under its own tenant's namespace and its own bridge id, so the topic is
// the one thing about a message its sender could not choose. Resolving the tenant
// from the store by bridge id instead ("the owner wins") would let a tenant name
// another tenant's bridge and have its event filed there.
//
// The registration, because a wide grant on device topics (mo.>) happens to reach
// one bridge-shaped topic per namespace with a made-up id. A sender nobody
// registered is nobody: the event is dropped, never adopted into the tenant.
//
// # What is not forwarded
//
// Keepalives and protocol negotiation belong to a connection, and the connection
// here is the Hub's own: forwarding a client's request to switch protocol would
// switch the Hub's stream to one it does not speak. A UID carrying the Hub's own
// marker prefix is refused, because those are the Hub's to draw. And an event
// already forwarded in the last minute is forwarded once: the export is QoS 1, so
// the broker may deliver it again.

// cotOutSuffix is how an export topic ends, for the one cheap test the bus
// handler makes before a message is queued.
const cotOutSuffix = "/tak/cot/out"

// Results of an exported event, as metric label values.
const (
	cotForwarded    = "forwarded"
	cotNowhere      = "nowhere" // accepted, and the tenant has no TAK server
	cotUnregistered = "unregistered"
	cotRateLimited  = "ratelimited"
	cotDuplicate    = "duplicate"
	cotControl      = "control" // a keepalive or protocol negotiation
	cotHubUID       = "hubuid"
	cotError        = "error"
)

// sourceClient labels events that came from a kit or an app.
const sourceClient = "client"

// bridgeCacheTTL is how long "is this bridge registered to this tenant" is
// reused. The export is frequent and the answer changes when somebody adds or
// removes a kit.
const bridgeCacheTTL = 30 * time.Second

// dedupWindow is how long an event is remembered so that a redelivery of it is
// not forwarded again.
const dedupWindow = time.Minute

// admit is the part of taking an exported event that runs on the bus's own
// goroutine. It must not block and does not: a size check, a topic split and two
// token buckets.
func (f *Forwarder) admit(topic string, payload []byte) bool {
	if len(payload) > tak.ClientLimits.MaxBytes {
		cotTotal.WithLabelValues(sourceClient, tak.ReasonSize).Inc()
		return false
	}
	tenantID, _, ok := hubmqtt.ParseTAKCotOut(topic)
	if !ok {
		// Ends like an export topic and is not one. Nothing subscribes this
		// forwarder to such a topic, so this is belt and braces.
		return false
	}
	now := time.Now()
	if looksUrgent(payload) &&
		f.limits.take("su|"+topic, senderUrgentRate, senderUrgentBurst, now) &&
		f.limits.take("tu|"+tenantID, tenantUrgentRate, tenantUrgentBurst, now) {
		return true
	}
	if f.limits.take("s|"+topic, senderRate, senderBurst, now) &&
		f.limits.take("t|"+tenantID, tenantRate, tenantBurst, now) {
		return true
	}
	cotTotal.WithLabelValues(sourceClient, cotRateLimited).Inc()
	return false
}

// looksUrgent is a guess, made before the payload is parsed, at whether it is an
// emergency. It only chooses which budget the event draws on first; whether the
// event really is one is decided after it has been parsed.
func looksUrgent(payload []byte) bool {
	return bytes.Contains(payload, []byte("<emergency")) ||
		bytes.Contains(payload, []byte(`type="b-a`)) ||
		bytes.Contains(payload, []byte(`type='b-a`))
}

// onCot takes one exported event through to the tenant's TAK servers.
func (f *Forwarder) onCot(r *run, tenantID, bridgeID string, payload []byte) {
	ctx := r.ctx
	switch f.bridgeRegistered(ctx, tenantID, bridgeID) {
	case regNo:
		cotTotal.WithLabelValues(sourceClient, cotUnregistered).Inc()
		f.log.Debug("takhosted: exported CoT from a bridge not registered to the tenant its topic names, dropped",
			"tenant", tenantID, "bridge", bridgeID)
		return
	case regUnknown:
		// The store could not say. Dropped rather than assumed: this is the check
		// that keeps one tenant's event out of another's server.
		cotTotal.WithLabelValues(sourceClient, cotError).Inc()
		return
	}

	now := time.Now()
	line, meta, err := tak.Sanitize(payload, tak.ClientLimits, now)
	if err != nil {
		reason := cotError
		var refused *tak.Refused
		if errors.As(err, &refused) {
			reason = refused.Reason
		}
		cotTotal.WithLabelValues(sourceClient, reason).Inc()
		f.log.Debug("takhosted: exported CoT refused", "tenant", tenantID, "bridge", bridgeID, "error", err)
		return
	}
	if isControlType(meta.Type) {
		cotTotal.WithLabelValues(sourceClient, cotControl).Inc()
		return
	}
	if isHubMarkerUID(meta.UID) {
		cotTotal.WithLabelValues(sourceClient, cotHubUID).Inc()
		f.log.Debug("takhosted: exported CoT carries a UID only the Hub draws, dropped",
			"tenant", tenantID, "bridge", bridgeID, "uid", meta.UID)
		return
	}
	if !r.seen.first(tenantID, line, now) {
		cotTotal.WithLabelValues(sourceClient, cotDuplicate).Inc()
		return
	}

	// A position event from the node itself: the Hub stops drawing that node from
	// its position reports while this one is current. Recorded whether or not the
	// tenant has a TAK server right now, so that turning one on does not start
	// with every exporting kit drawn twice.
	if strings.HasPrefix(meta.Type, "a-") {
		r.suppress.mark(tenantID, meta.UID, meta.Stale, now)
	}

	ups := f.upstreams(ctx, tenantID)
	if len(ups) == 0 {
		cotTotal.WithLabelValues(sourceClient, cotNowhere).Inc()
		return
	}
	line = append(line, '\n')
	for _, up := range ups {
		if up == nil {
			continue
		}
		f.enqueue(r, tenantID, up, line, meta.Emergency)
	}
	cotTotal.WithLabelValues(sourceClient, cotForwarded).Inc()
}

// isControlType reports whether a CoT type is connection housekeeping rather
// than something to draw: a keepalive (t-x-c-t and its reply) or TAK protocol
// negotiation (t-x-takp-...).
func isControlType(typ string) bool {
	return strings.HasPrefix(typ, tak.TypeKeepalive) || strings.HasPrefix(typ, "t-x-takp")
}

// --- is this bridge registered to this tenant --------------------------------

type registration int

const (
	regUnknown registration = iota
	regYes
	regNo
)

type regEntry struct {
	is     registration
	expiry time.Time
}

// bridgeRegistered asks the store, by tenant AND bridge id, with a short cache.
//
// By both, on purpose. A bridge id is unique within a tenant, not across them, so
// "which tenant owns this id" has no single answer and is the wrong question
// anyway: the right one is whether the tenant the topic names has this bridge.
func (f *Forwarder) bridgeRegistered(ctx context.Context, tenantID, bridgeID string) registration {
	if f.store == nil || tenantID == "" || bridgeID == "" {
		return regNo
	}
	key := tenantID + "|" + bridgeID
	now := time.Now()
	f.regMu.Lock()
	if e, ok := f.reg[key]; ok && now.Before(e.expiry) {
		f.regMu.Unlock()
		return e.is
	}
	f.regMu.Unlock()

	is := regYes
	b, err := f.store.GetBridge(ctx, tenantID, bridgeID)
	switch {
	case err == nil && b != nil:
	case err == nil, errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrNotFound):
		is = regNo
	default:
		// Not cached: a store blip must not keep a kit's events out for the
		// whole TTL after it has passed.
		f.log.Warn("takhosted: could not check a bridge's registration", "tenant", tenantID, "error", err)
		return regUnknown
	}
	f.regMu.Lock()
	f.reg[key] = regEntry{is: is, expiry: now.Add(bridgeCacheTTL)}
	f.regMu.Unlock()
	return is
}

func (f *Forwarder) pruneRegistrations(now time.Time) {
	f.regMu.Lock()
	defer f.regMu.Unlock()
	for k, e := range f.reg {
		if !now.Before(e.expiry) {
			delete(f.reg, k)
		}
	}
}

func (f *Forwarder) forgetRegistrations(tenantID string) {
	prefix := tenantID + "|"
	f.regMu.Lock()
	defer f.regMu.Unlock()
	for k := range f.reg {
		if strings.HasPrefix(k, prefix) {
			delete(f.reg, k)
		}
	}
}

// --- forwarded once ----------------------------------------------------------

// dedup remembers what was forwarded in the last dedupWindow, per tenant.
//
// In the leader's memory and not a store claim. The forwarder is a leader
// singleton, so there is no second replica to race, and a database write per
// exported event would put Postgres on a path every kit takes twice a minute. A
// change of leader forgets it, and the cost of that is one event written twice to
// a server that keys markers by UID.
type dedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newDedup() *dedup { return &dedup{seen: map[string]time.Time{}} }

// first reports whether this event has not been seen for this tenant inside the
// window, and remembers it.
func (d *dedup) first(tenantID string, line []byte, now time.Time) bool {
	sum := sha256.Sum256(line)
	key := tenantID + "|" + hex.EncodeToString(sum[:12])
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.seen[key]; ok && now.Sub(at) < dedupWindow {
		return false
	}
	d.seen[key] = now
	return true
}

func (d *dedup) prune(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, at := range d.seen {
		if now.Sub(at) >= dedupWindow {
			delete(d.seen, k)
		}
	}
}
