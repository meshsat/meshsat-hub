package takhosted

import (
	"errors"
	"strconv"
	"sync"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// The return path (MESHSAT-1461): TAK traffic delivered to a tenant's kits and
// apps over the MQTT session they already hold.
//
// # Two sources, two topics, one direction each
//
// An event read from one of the tenant's TAK servers goes to the tenant's
// clients, on {ns}/broadcast/tak/cot/in. It goes NOWHERE else: not to the
// tenant's other TAK server, if it has two. The Hub is not a federation link
// between a customer's servers, and bridging them here would be a loop the first
// time the two were federated with each other.
//
// An event one of the tenant's clients exported goes to the tenant's TAK servers
// (cot_ingest.go) and to the tenant's OTHER clients, on .../in/{sender}. The
// second half is not optional once the Hub is the only thing that talks to a TAK
// server: a server does not send an event back down the connection it arrived
// on, and the Hub is one connection for all of a tenant's clients, so two kits
// would never see each other through it. It also means a tenant with no TAK
// server at all still gets a shared picture between its own clients.
//
// # Never retained, never queued
//
// QoS 0 and not retained. A position is true for a minute or two. A broker that
// queued it for a client that was offline, or kept it to hand to the next one to
// subscribe, would draw where somebody was as where they are.
//
// # What is dropped on the way in
//
// Everything goes through tak.Sanitize, with the larger limits a server's
// traffic needs: a tenant's own server is the tenant's, but what it relays came
// from whoever is connected to it. Then: keepalives and protocol negotiation,
// which belong to the connection; the Hub's own UIDs; an echo of what the Hub
// wrote in the last minute (a server is not supposed to send that back, and
// "not supposed to" is not a property to build on for servers the Hub has never
// met); and a second copy of an event already delivered, which is what a tenant
// with two servers that share traffic would produce.

const (
	sourceUpstream = "upstream"

	cotDelivered = "delivered"
	cotEcho      = "echo"

	// What one connection may deliver. A busy team is a few events a second;
	// this is room for a server replaying its picture after a reconnect, and a
	// ceiling on what a server can make the Hub publish.
	upstreamRate, upstreamBurst = 50.0, 500.0

	// echoWindow is how long the Hub remembers what it wrote to a tenant's
	// servers.
	echoWindow = time.Minute
)

var errNoBus = errors.New("takhosted: no message bus")

// onUpstream takes one frame read from a tenant's TAK server through to the
// tenant's clients. It runs on that connection's reader goroutine.
func (f *Forwarder) onUpstream(r *run, tenantID string, up *takfront.Tenant, frame []byte) {
	now := time.Now()
	if !f.limits.take("u|"+upstreamKey(tenantID, up), upstreamRate, upstreamBurst, now) {
		cotTotal.WithLabelValues(sourceUpstream, cotRateLimited).Inc()
		return
	}
	line, meta, err := tak.Sanitize(frame, tak.UpstreamLimits, now)
	if err != nil {
		reason := cotError
		var refused *tak.Refused
		if errors.As(err, &refused) {
			reason = refused.Reason
		}
		cotTotal.WithLabelValues(sourceUpstream, reason).Inc()
		f.log.Debug("takhosted: an event from a tenant's TAK server was refused",
			"tenant", tenantID, "kind", up.Label, "error", err)
		return
	}
	switch {
	case isControlType(meta.Type):
		cotTotal.WithLabelValues(sourceUpstream, cotControl).Inc()
		return
	case isHubMarkerUID(meta.UID):
		cotTotal.WithLabelValues(sourceUpstream, cotHubUID).Inc()
		return
	case r.sent.has(tenantID, meta.UID, meta.Time, now):
		cotTotal.WithLabelValues(sourceUpstream, cotEcho).Inc()
		return
	case !r.seen.first(tenantID, line, now):
		cotTotal.WithLabelValues(sourceUpstream, cotDuplicate).Inc()
		return
	}
	if err := f.deliver(hubmqtt.TopicTAKBroadcastFor(tenantID), line); err != nil {
		cotTotal.WithLabelValues(sourceUpstream, cotError).Inc()
		f.log.Debug("takhosted: could not deliver a TAK event to the tenant's clients", "tenant", tenantID, "error", err)
		return
	}
	cotTotal.WithLabelValues(sourceUpstream, cotDelivered).Inc()
}

// deliver publishes one event to clients: QoS 0, never retained.
func (f *Forwarder) deliver(topic string, line []byte) error {
	if f.bus == nil {
		return errNoBus
	}
	return f.bus.Publish(topic, 0, false, line)
}

// --- what the Hub wrote, so its echo is recognised ---------------------------

// sentMemory remembers the events the Hub wrote to a tenant's servers, by UID and
// event time, for echoWindow.
type sentMemory struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newSentMemory() *sentMemory { return &sentMemory{at: map[string]time.Time{}} }

func sentKey(tenantID, uid string, eventTime time.Time) string {
	return tenantID + "|" + uid + "|" + strconv.FormatInt(eventTime.Unix(), 10)
}

func (s *sentMemory) add(tenantID, uid string, eventTime, now time.Time) {
	s.mu.Lock()
	s.at[sentKey(tenantID, uid, eventTime)] = now
	s.mu.Unlock()
}

func (s *sentMemory) has(tenantID, uid string, eventTime, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.at[sentKey(tenantID, uid, eventTime)]
	return ok && now.Sub(at) < echoWindow
}

func (s *sentMemory) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, at := range s.at {
		if now.Sub(at) >= echoWindow {
			delete(s.at, k)
		}
	}
}
