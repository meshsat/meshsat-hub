package takhosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
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
// # The loop guard is not optional
//
// The OTS poller mirrors TAK markers back into the devices table as type "tak",
// and those markers include the ones this forwarder just sent. Forwarding them
// again would build a loop that grows with every cycle. Two independent guards
// stop it: a device whose type is an artefact type is never forwarded, and a UID
// carrying the Hub's own marker prefix is never forwarded either.
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

	mu    sync.Mutex
	conns map[string]*upstreamConn
}

// upstreamConn is one open CoT connection to a tenant's server.
//
// Held open rather than dialled per position: every connection forks a process in
// OpenTAKServer, and a kit reporting every few minutes would otherwise fork one
// each time.
type upstreamConn struct {
	conn net.Conn
	last time.Time
}

// hubMarkerPrefixes are the UIDs the Hub itself publishes. A marker carrying one
// came from us, so forwarding it would echo.
var hubMarkerPrefixes = []string{"meshsat-bridge-", "meshsat-device-"}

// DefaultStaleSec is how long a forwarded position is treated as current. Two
// minutes: long enough that a kit reporting every minute never flickers, short
// enough that a stale position fades rather than sitting on the map as fact.
const DefaultStaleSec = 120

// idleUpstream closes a tenant's connection after this long with nothing to send,
// so a tenant with no active kit does not hold a process open in its own server.
const idleUpstream = 15 * time.Minute

// NewForwarder wires one up. dial is normally takfront.Server.DialTenant and
// tenant is normally DirectoryRefresher.TenantByID.
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
		staleSec: DefaultStaleSec,
		conns:    map[string]*upstreamConn{},
	}
}

// Run subscribes and forwards until ctx is done. Registered with
// leaderSingletons, so only the lease holder is here.
func (f *Forwarder) Run(ctx context.Context) {
	if f.bus == nil || !f.bus.IsConnected() {
		f.log.Warn("takhosted: no message bus, device positions will not reach any TAK server")
		return
	}

	// DualFilters, or only the default tenant is ever seen: meshsat/+/position
	// does not match meshsat/{tenant}/{device}/position, and every other tenant's
	// kit would be silently absent from its own map.
	//
	// The filters are written out rather than built with hubmqtt.TopicPosition("+").
	// Since MESHSAT-1022 every builder runs its id through EncodeSegment, because a
	// phone number is a device id and starts with the MQTT wildcard. So "+" is
	// precisely what those builders encode, and TopicPosition("+") returns
	// "meshsat/%2B/position": a filter matching a device literally named "+" and
	// nothing else. It subscribes without error, this function logs that it is
	// forwarding, and not one position ever arrives. Every other subscriber in the
	// Hub spells its wildcard filter out for the same reason -- see
	// internal/position/subscriber.go, internal/sos/detector.go and
	// internal/aprsis/subscriber.go. Do not "tidy" these back into the builders.
	for _, legacy := range []string{
		"meshsat/+/position",
		"meshsat/+/sos",
	} {
		for _, filter := range hubmqtt.DualFilters(legacy) {
			fl := filter
			if err := f.bus.Subscribe(fl, 0, func(topic string, payload []byte) {
				f.onPosition(ctx, topic, payload)
			}); err != nil {
				f.log.Warn("takhosted: could not subscribe for TAK forwarding", "filter", fl, "error", err)
			}
		}
	}
	f.log.Info("takhosted: forwarding device positions to tenant TAK servers")

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.closeAll()
			return
		case <-t.C:
			f.reapIdle()
		}
	}
}

// onPosition forwards one position to its own tenant's server.
func (f *Forwarder) onPosition(ctx context.Context, topic string, payload []byte) {
	tenantID, deviceID, _, ok := hubmqtt.ParseDeviceTopic(topic)
	if !ok {
		f.log.Debug("takhosted: unparseable device topic, not forwarded", "topic", topic)
		return
	}
	if isHubMarkerUID(deviceID) {
		// Our own marker, mirrored back by the OTS poller. Forwarding it is the
		// loop.
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
	// row -- a marker the poller mirrored -- must never be sent back.
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

	// Every upstream is attempted, and one failing must not stop the others: a
	// customer's own server being unreachable is no reason to keep their kit off
	// their hosted map, nor the reverse.
	for _, up := range ups {
		if up == nil {
			continue
		}
		if err := f.send(ctx, tenantID, up, xml); err != nil {
			f.log.Warn("takhosted: forwarding a position failed",
				"tenant", tenantID, "upstream", up.Upstream, "kind", up.Label, "error", err)
		}
	}
}

// send writes one CoT event to the tenant's server, dialling on first use and
// redialling once if the held connection has gone.
func (f *Forwarder) send(ctx context.Context, tenantID string, tenant *takfront.Tenant, xml []byte) error {
	payload := append(append([]byte{}, xml...), '\n')

	// Keyed by tenant AND upstream address: a tenant can now have more than one
	// server, and keying by tenant alone would make two upstreams share one
	// connection and send each position to whichever was dialled first.
	key := upstreamKey(tenantID, tenant)

	for attempt := 0; attempt < 2; attempt++ {
		c, err := f.connFor(ctx, key, tenant)
		if err != nil {
			return err
		}
		_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.conn.Write(payload); err != nil {
			// The upstream went away: drop it and try once more with a fresh one.
			f.drop(key)
			if attempt == 1 {
				forwardsFailed.WithLabelValues(kindOf(tenant), reasonWrite).Inc()
				return fmt.Errorf("write to %s: %w", tenant.Upstream, err)
			}
			continue
		}
		f.mu.Lock()
		c.last = time.Now()
		f.mu.Unlock()
		return nil
	}
	return errors.New("takhosted: could not write to the tenant's TAK server")
}

// upstreamKey identifies one connection: a tenant can have several upstreams now,
// so the tenant alone is not enough. Keyed on the address rather than the label,
// because the address is what a connection actually goes to.
func upstreamKey(tenantID string, t *takfront.Tenant) string {
	return tenantID + "|" + t.Upstream
}

func (f *Forwarder) connFor(ctx context.Context, key string, tenant *takfront.Tenant) (*upstreamConn, error) {
	f.mu.Lock()
	if c, ok := f.conns[key]; ok {
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	conn, err := f.dial(ctx, tenant)
	if err != nil {
		forwardsFailed.WithLabelValues(kindOf(tenant), reasonDial).Inc()
		return nil, fmt.Errorf("dial %s: %w", tenant.Upstream, err)
	}
	// A successful dial is not a working connection (MESHSAT-1066). Under TLS 1.3
	// the server receives our certificate after it has finished its own handshake,
	// so it refuses us on a connection we already believe is open. Find that out
	// HERE: before the connection is cached, and before the log below claims it
	// opened.
	if why := upstreamRejected(conn); why != nil {
		_ = conn.Close()
		forwardsFailed.WithLabelValues(kindOf(tenant), reasonRefused).Inc()
		return nil, fmt.Errorf("%s refused this Hub: %w", tenant.Upstream, why)
	}
	c := &upstreamConn{conn: conn, last: time.Now()}

	f.mu.Lock()
	// Another goroutine may have dialled while this one was waiting.
	if existing, ok := f.conns[key]; ok {
		f.mu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	f.conns[key] = c
	f.mu.Unlock()
	// The tenant and the address, not the key: a reader wants to know whose server
	// this is and where it is, and the key is an implementation detail.
	f.log.Info("takhosted: opened a CoT connection to a tenant's TAK server",
		"tenant", tenant.TenantID, "upstream", tenant.Upstream, "kind", tenant.Label)
	return c, nil
}

func (f *Forwarder) drop(key string) {
	f.mu.Lock()
	c, ok := f.conns[key]
	delete(f.conns, key)
	f.mu.Unlock()
	if ok {
		_ = c.conn.Close()
	}
}

func (f *Forwarder) reapIdle() {
	cutoff := time.Now().Add(-idleUpstream)
	f.mu.Lock()
	var stale []string
	for id, c := range f.conns {
		if c.last.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	f.mu.Unlock()
	for _, id := range stale {
		f.log.Info("takhosted: closing an idle CoT connection", "tenant", id)
		f.drop(id)
	}
}

func (f *Forwarder) closeAll() {
	f.mu.Lock()
	conns := f.conns
	f.conns = map[string]*upstreamConn{}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.conn.Close()
	}
}

// isArtefact reports whether this device row is one an integration made rather
// than something a customer registered. Those are mirrored TAK markers, and
// sending them back is the loop this guards against.
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
