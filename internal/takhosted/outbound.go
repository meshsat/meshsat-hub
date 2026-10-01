package takhosted

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Forwarder sends a tenant's own device positions into that tenant's
// OpenTAKServer, so a customer's satellite kit appears on their ATAK map beside
// their phones (MESHSAT-1037).
//
// # A leader singleton, unlike the directory refresher
//
// The refresher runs everywhere because the front is a door and every replica
// must accept phones. This is the opposite: it WRITES, and two replicas
// forwarding the same position would put every device on the map twice. So it is
// registered with leaderSingletons and only the lease holder runs it.
//
// # Subscribed once, gated on the run
//
// The bus has no unsubscribe, and Run is called again on every acquisition of the
// lease. Subscribing inside Run therefore left one more handler behind each time,
// bound to a context that had been cancelled: a replica that lost the lease
// logged a failure for every position it could no longer forward (MESHSAT-1457),
// and one that lost it and won it back wrote every position twice, the second
// copy through the stale handler and the new run's connection. So the handlers
// are installed once for the life of the process, and each reads the current run
// from an atomic pointer: nil when this replica does not hold the lease, in which
// case the message is simply not this replica's to forward.
//
// # Nothing here blocks the bus
//
// See link.go. A handler hands its message to the dispatcher and returns.
//
// # The loop guard
//
// A marker this forwarder draws must never be forwarded again. Two independent
// guards hold that: a UID carrying the Hub's own marker prefix is never
// forwarded, and neither is a device row of an artefact type. The poller that
// used to mirror TAK markers into the devices table is gone (MESHSAT-1032), so
// nothing creates such rows today; the guards stay, because the return path is
// being rebuilt and they are what makes that safe.
type Forwarder struct {
	bus   bus.MessageBus
	store store.Store
	dial  func(ctx context.Context, t *takfront.Tenant) (net.Conn, error)
	// upstreams returns EVERY server this tenant's CoT should reach: the hosted
	// instance the operator runs for them, the server they run themselves
	// (MESHSAT-1065), or both.
	//
	// A slice rather than one upstream, because choosing would quietly break
	// whichever was not chosen. Prefer the customer's own server and their
	// satellite kit disappears from the hosted map their phones are looking at;
	// prefer the hosted one and nothing ever arrives on the server they just
	// configured. Both are surprising in a way no error message would explain.
	upstreams func(ctx context.Context, tenantID string) []*takfront.Tenant
	log       *slog.Logger

	// staleSec is how long a forwarded position stays current on an ATAK map.
	staleSec int

	// subscribed is the filters already installed on the bus. Guarded by subMu.
	subMu      sync.Mutex
	subscribed map[string]bool

	// cur is the run the handlers feed, nil while this replica is not the leader.
	cur atomic.Pointer[run]

	// limits are the budgets on exported CoT, drawn on by the bus handler.
	limits *limiter

	// reg caches whether a bridge is registered to a tenant. Guarded by regMu.
	regMu sync.Mutex
	reg   map[string]regEntry
}

// run is one tenure as leader: the queue the handlers feed and the links it
// opened. Nothing in it outlives the lease.
type run struct {
	ctx    context.Context
	intake chan inbound

	mu    sync.Mutex
	links map[string]*link
	wg    sync.WaitGroup

	// suppress is the nodes that export their own marker; seen is what has been
	// forwarded or delivered in the last minute; sent is what was written to TAK
	// servers, so its echo is recognised; present is the tenants with a client
	// online. All four belong to this tenure and start empty.
	suppress *suppressor
	seen     *dedup
	sent     *sentMemory
	present  *presence
}

// inbound is one thing waiting for the dispatcher: a bus message, or (when
// connect is set) a tenant whose connections want opening.
type inbound struct {
	topic   string
	payload []byte
	connect string
}

// intakeQueue bounds what the handlers may have waiting for the dispatcher. The
// dispatcher reads the store, so it can fall behind a burst; a handler never
// waits for it.
const intakeQueue = 1024

// forwardFilters are the legacy shapes of what is forwarded. Written out rather
// than built with hubmqtt.TopicPosition("+"): since MESHSAT-1022 every builder
// runs its id through EncodeSegment, because a phone number is a device id and
// starts with the MQTT wildcard. So "+" is precisely what those builders encode,
// and TopicPosition("+") returns "meshsat/%2B/position": a filter matching a
// device literally named "+" and nothing else. It subscribes without error, Run
// logs that it is forwarding, and not one position ever arrives. Every other
// subscriber in the Hub spells its wildcard filter out for the same reason -- see
// internal/position/subscriber.go, internal/sos/detector.go and
// internal/aprsis/subscriber.go. Do not "tidy" these back into the builders.
var forwardFilters = []string{
	"meshsat/+/position",
	"meshsat/+/sos",
}

// presenceFilters are what says a tenant has a client online: every bridge's
// health, every thirty seconds, and its birth. See presence.go.
var presenceFilters = []string{
	"meshsat/bridge/+/health",
	"meshsat/bridge/+/birth",
}

// hubMarkerPrefixes are the UIDs the Hub itself publishes: the markers it draws
// for bridges and devices, and its own hello and ping on a TAK stream. An event
// carrying one came from us, so forwarding it would echo, and a client or a
// server sending one is speaking in the Hub's name.
var hubMarkerPrefixes = []string{"meshsat-bridge-", "meshsat-device-", hubUIDPrefix}

// DefaultStaleSec is how long a forwarded position is treated as current. Two
// minutes: long enough that a kit reporting every minute never flickers, short
// enough that a stale position fades rather than sitting on the map as fact.
const DefaultStaleSec = 120

// idleUpstream closes a tenant's connection after this long with nothing to send,
// so a tenant with no active kit does not hold a process open in its own server.
//
// Held open in between rather than dialled per position: every connection forks a
// process in OpenTAKServer, and a kit reporting every few minutes would otherwise
// fork one each time.
const idleUpstream = 15 * time.Minute

// NewForwarder wires one up. dial is normally takfront.DialUpstream and upstreams
// is normally Upstreams.For.
func NewForwarder(
	b bus.MessageBus,
	s store.Store,
	dial func(ctx context.Context, t *takfront.Tenant) (net.Conn, error),
	upstreams func(ctx context.Context, tenantID string) []*takfront.Tenant,
	log *slog.Logger,
) *Forwarder {
	if log == nil {
		log = slog.Default()
	}
	return &Forwarder{
		bus: b, store: s, dial: dial, upstreams: upstreams, log: log,
		staleSec:   DefaultStaleSec,
		subscribed: map[string]bool{},
		limits:     newLimiter(),
		reg:        map[string]regEntry{},
	}
}

// Run forwards until ctx is done. Registered with leaderSingletons, so only the
// lease holder is here, and it is called again each time the lease is acquired.
func (f *Forwarder) Run(ctx context.Context) {
	if f.bus == nil || !f.bus.IsConnected() {
		f.log.Warn("takhosted: no message bus, device positions will not reach any TAK server")
		return
	}

	r := &run{
		ctx: ctx, intake: make(chan inbound, intakeQueue), links: map[string]*link{},
		suppress: newSuppressor(), seen: newDedup(), sent: newSentMemory(), present: newPresence(),
	}
	// The run is published BEFORE the filters are installed, so what the broker
	// delivers the moment a filter lands -- retained positions -- has somewhere
	// to go.
	f.cur.Store(r)
	f.subscribe()
	f.log.Info("takhosted: forwarding device positions to tenant TAK servers")

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		f.dispatch(r)
	}()

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Handlers stop feeding this run first, then its links are closed and
			// waited for, so nothing of this tenure is still writing when the next
			// leader starts.
			f.cur.CompareAndSwap(r, nil)
			r.closeAll()
			r.wg.Wait()
			return
		case <-t.C:
			now := time.Now()
			r.reapIdle(f.log, now)
			r.suppress.prune(now)
			r.seen.prune(now)
			r.sent.prune(now)
			f.limits.prune(now)
			f.pruneRegistrations(now)
			// Every tenant with a client online has its connections checked: a
			// server that was down, or a hosted instance whose identity had not
			// been issued yet, is tried again here.
			for _, tenantID := range r.present.active(now) {
				r.askConnect(tenantID)
			}
		}
	}
}

// subscribe installs each filter that is not installed yet.
//
// Once per filter for the life of the process, not once per Run: see the type's
// comment. A filter that failed is tried again on the next acquisition.
//
// DualFilters, or only the default tenant is ever seen: meshsat/+/position does
// not match meshsat/{tenant}/{device}/position, and every other tenant's kit
// would be silently absent from its own map.
func (f *Forwarder) subscribe() {
	f.subMu.Lock()
	defer f.subMu.Unlock()
	var filters []string
	for _, legacy := range forwardFilters {
		filters = append(filters, hubmqtt.DualFilters(legacy)...)
	}
	// What kits and apps export themselves (MESHSAT-1458); see cot_ingest.go.
	filters = append(filters, hubmqtt.TAKCotOutFilters()...)
	// Which tenants have a client online (MESHSAT-1461); see presence.go.
	for _, legacy := range presenceFilters {
		filters = append(filters, hubmqtt.DualFilters(legacy)...)
	}
	for _, filter := range filters {
		if f.subscribed[filter] {
			continue
		}
		if err := f.bus.Subscribe(filter, 0, f.receive); err != nil {
			f.log.Warn("takhosted: could not subscribe for TAK forwarding", "filter", filter, "error", err)
			continue
		}
		f.subscribed[filter] = true
	}
}

// receive is what the bus calls. It runs on the bus's inbound goroutine, so it
// does nothing but hand the message over: no store read, no dial, no write.
func (f *Forwarder) receive(topic string, payload []byte) {
	r := f.cur.Load()
	if r == nil {
		// Not the leader. The message is the leader's to forward.
		return
	}
	switch {
	case strings.HasSuffix(topic, cotOutSuffix):
		if !f.admit(topic, payload) {
			return
		}
	case strings.HasSuffix(topic, "/health"), strings.HasSuffix(topic, "/birth"):
		// A bridge reporting in: its tenant has a client online. Nothing of the
		// message is kept. Only a tenant that has just arrived is handed to the
		// dispatcher, so its connections open now and not at the next tick.
		if tenantID, _, rest, ok := hubmqtt.ParseBridgeTopic(topic); ok && len(rest) == 1 {
			if r.present.touch(tenantID, time.Now()) {
				r.askConnect(tenantID)
			}
		}
		return
	}
	// Copied: the handler returns before the dispatcher reads it.
	m := inbound{topic: topic, payload: append([]byte(nil), payload...)}
	select {
	case r.intake <- m:
	default:
		intakeDropped.Inc()
	}
}

// dispatch turns queued messages into events until the run ends.
func (f *Forwarder) dispatch(r *run) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case m := <-r.intake:
			if m.connect != "" {
				f.connectTenant(r, m.connect)
				continue
			}
			if tenantID, bridgeID, ok := hubmqtt.ParseTAKCotOut(m.topic); ok {
				f.onCot(r, tenantID, bridgeID, m.payload)
				continue
			}
			f.onPosition(r, m.topic, m.payload)
		}
	}
}

// onPosition forwards one position to its own tenant's servers.
func (f *Forwarder) onPosition(r *run, topic string, payload []byte) {
	ctx := r.ctx
	tenantID, deviceID, suffix, ok := hubmqtt.ParseDeviceTopic(topic)
	if !ok {
		f.log.Debug("takhosted: unparseable device topic, not forwarded", "topic", topic)
		return
	}
	if isHubMarkerUID(deviceID) {
		// Our own marker. Forwarding it is the loop.
		return
	}
	if r.suppress.suppressed(tenantID, deviceID, time.Now()) {
		// This node exports its own marker, which is the better one. Drawing it
		// from its position report as well would put it on the map twice.
		return
	}

	var pos store.Position
	if err := json.Unmarshal(payload, &pos); err != nil {
		f.log.Debug("takhosted: position payload did not decode", "topic", topic, "error", err)
		return
	}
	if pos.Lat == 0 && pos.Lon == 0 {
		// Null Island is not a position. Devices report it when they have no fix,
		// and putting it on a map is worse than leaving the device absent.
		return
	}

	ups := f.upstreams(ctx, tenantID)
	if len(ups) == 0 {
		// Nowhere to send: no hosted instance that is Ready, and no server of the
		// tenant's own. Not an error -- most tenants will never turn TAK on.
		return
	}

	// The device's own type decides whether it is forwardable at all. An artefact
	// row -- a marker an integration mirrored -- must never be sent back.
	if f.isArtefact(ctx, tenantID, deviceID) {
		return
	}

	callsign := deviceID
	if d, err := f.store.GetDevice(ctx, tenantID, deviceID); err == nil && d != nil && d.Label != "" {
		callsign = d.Label
	}

	ev := tak.BuildPositionEvent(
		"meshsat-device-"+deviceID, callsign,
		pos.Lat, pos.Lon, pos.Alt, f.staleSec, pos.Source,
	)
	xml, err := tak.MarshalCotEvent(ev)
	if err != nil {
		f.log.Warn("takhosted: could not render CoT", "device", deviceID, "error", err)
		return
	}
	line := append(xml, '\n')
	if at, err := time.Parse(time.RFC3339, ev.Time); err == nil {
		// Remembered, so a server that sends this back is not delivering news.
		r.sent.add(tenantID, ev.UID, at, time.Now())
	}

	// Every upstream gets it, and one failing must not stop the others: a
	// customer's own server being unreachable is no reason to keep their kit off
	// their hosted map, nor the reverse. Each has its own link, so they cannot
	// even wait for one another.
	for _, up := range ups {
		if up == nil {
			continue
		}
		// An SOS report goes ahead of routine positions on its link.
		f.enqueue(r, tenantID, up, line, suffix == "sos")
	}
}

// enqueue hands one event to the link for one upstream, making the link on first
// use. It never waits: a full queue loses its oldest event, and that is counted.
func (f *Forwarder) enqueue(r *run, tenantID string, up *takfront.Tenant, line []byte, urgent bool) {
	// Keyed by tenant AND upstream address: a tenant can have more than one
	// server, and keying by tenant alone would make two upstreams share one
	// connection and send each position to whichever was dialled first.
	key := upstreamKey(tenantID, up)

	// Held across the send on purpose: the send cannot block, and holding it is
	// what stops an event landing in the queue of a link the reaper has just
	// removed.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	l := f.linkLocked(r, key, tenantID, up)
	j := job{up: up, payload: line}
	// An emergency has a queue of its own, which the link empties first, so it
	// neither waits behind routine positions nor is the one displaced by them.
	q := l.q
	if urgent {
		q = l.urgent
	}
	select {
	case q <- j:
		return
	default:
	}
	// Full: this upstream is not keeping up. The OLDEST waiting event is the one
	// to lose, because a position is worth only its freshness. Nothing else puts
	// into this queue while r.mu is held, so after taking one out there is room;
	// if the link emptied it in between, there is room anyway.
	select {
	case <-q:
	default:
	}
	select {
	case q <- j:
	default:
	}
	forwardsFailed.WithLabelValues(kindOf(up), reasonQueue).Inc()
}

// linkLocked returns the link for one upstream, making and starting it on first
// use, and records the upstream as it was just resolved. r.mu must be held.
func (f *Forwarder) linkLocked(r *run, key, tenantID string, up *takfront.Tenant) *link {
	l, ok := r.links[key]
	if !ok {
		l = newLink(r, f, key, tenantID)
		r.links[key] = l
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			l.loop()
		}()
	}
	l.setUp(up)
	return l
}

// askConnect queues a tenant for connectTenant. It never waits: it is called
// from the bus handler.
func (r *run) askConnect(tenantID string) {
	select {
	case r.intake <- inbound{connect: tenantID}:
	default:
		// The dispatcher is behind. The next tick asks again.
	}
}

// connectTenant opens a connection to every TAK server of a tenant that has a
// client online, with nothing to send, so that what those servers have to say is
// heard. A tenant with no TAK server costs one lookup and nothing else.
func (f *Forwarder) connectTenant(r *run, tenantID string) {
	for _, up := range f.upstreams(r.ctx, tenantID) {
		if up == nil {
			continue
		}
		r.mu.Lock()
		if r.ctx.Err() != nil {
			r.mu.Unlock()
			return
		}
		l := f.linkLocked(r, upstreamKey(tenantID, up), tenantID, up)
		r.mu.Unlock()
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

// upstreamKey identifies one link: a tenant can have several upstreams, so the
// tenant alone is not enough. Keyed on the address rather than the label, because
// the address is what a connection actually goes to.
func upstreamKey(tenantID string, t *takfront.Tenant) string {
	return tenantID + "|" + t.Upstream
}

// reapIdle closes the links that have had nothing to send for idleUpstream and
// whose tenant has no client online to listen for.
func (r *run) reapIdle(log *slog.Logger, now time.Time) {
	cutoff := now.Add(-idleUpstream)
	var stale []*link
	r.mu.Lock()
	for key, l := range r.links {
		if r.present.present(l.tenantID, now) {
			continue
		}
		if l.idleSince().Before(cutoff) && len(l.q) == 0 && len(l.urgent) == 0 {
			delete(r.links, key)
			stale = append(stale, l)
		}
	}
	r.mu.Unlock()
	for _, l := range stale {
		log.Info("takhosted: closing an idle CoT connection", "tenant", l.key)
		l.close()
	}
}

// forget closes every link of one tenant.
func (r *run) forget(tenantID string) {
	var gone []*link
	r.mu.Lock()
	for key, l := range r.links {
		if l.tenantID == tenantID {
			delete(r.links, key)
			gone = append(gone, l)
		}
	}
	r.mu.Unlock()
	for _, l := range gone {
		l.close()
	}
}

func (r *run) closeAll() {
	r.mu.Lock()
	links := r.links
	r.links = map[string]*link{}
	r.mu.Unlock()
	for _, l := range links {
		l.close()
	}
}

// ForgetTenant satisfies tenancy.TenantForgetter: a tenant that has been purged
// or closed must not keep a connection open to its TAK server, nor one to a
// server whose settings have just been removed (MESHSAT-1460). The link is made
// again on the tenant's next event, from whatever its upstreams are by then.
func (f *Forwarder) ForgetTenant(tenantID string) {
	if f == nil {
		return
	}
	f.forgetRegistrations(tenantID)
	f.limits.forget("t|" + tenantID)
	f.limits.forget("tu|" + tenantID)
	f.limits.forget("u|" + tenantID + "|")
	if r := f.cur.Load(); r != nil {
		// Presence first: a link closed while its tenant still counts as present
		// would be opened again at the next tick.
		r.present.forget(tenantID)
		r.forget(tenantID)
		r.suppress.forget(tenantID)
	}
}

// isArtefact reports whether this device row is one an integration made rather
// than something a customer registered. Those are mirrored markers, and sending
// them back is the loop this guards against.
func (f *Forwarder) isArtefact(ctx context.Context, tenantID, deviceID string) bool {
	if f.store == nil {
		return false
	}
	d, err := f.store.GetDevice(ctx, tenantID, deviceID)
	if err != nil || d == nil {
		// Unknown device: forward it. A position arriving for a device the store
		// has not caught up with is not a reason to drop it off the map.
		return false
	}
	for _, t := range store.ArtefactDeviceTypes {
		if d.Type == t {
			return true
		}
	}
	return false
}

// isHubMarkerUID reports whether a UID is one the Hub publishes itself.
func isHubMarkerUID(uid string) bool {
	for _, p := range hubMarkerPrefixes {
		if strings.HasPrefix(uid, p) {
			return true
		}
	}
	return false
}

// TenantByID lets the forwarder find a tenant the refresher has already built,
// so there is one source of truth for which tenants are reachable and the
// immutable takfront.Directory needs no new accessor.
//
// It returns nil for a tenant with no TAK server or one that is not Ready, which
// the forwarder treats as "nothing to do" rather than an error: most tenants will
// never turn TAK on.
func (r *DirectoryRefresher) TenantByID(tenantID string) *takfront.Tenant {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[tenantID]
	if !ok {
		return nil
	}
	cp := t
	return &cp
}
