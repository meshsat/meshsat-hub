package takhosted

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// One TAK server, one goroutine (MESHSAT-1460).
//
// # Why the network moved off the bus
//
// The bus delivers in order on a single inbound goroutine, and the forwarder used
// to dial and write from inside its handler: up to ten seconds for the dial, half
// a second for the probe, ten more for the write. A tenant's own TAK server is an
// address a customer typed, so one of them pointing at something that never
// answers stalled the leader's whole MQTT intake for that long per position --
// every subscriber behind it, SOS detection included. Nothing a customer
// configures may be able to do that.
//
// So a handler now only hands its message over, the dispatcher builds the event,
// and each upstream is written by a goroutine of its own behind a bounded queue.
// A server that is slow or gone costs its own tenant its own events and nothing
// else.
//
// # Why a failed dial backs off
//
// The old code dialled once per position. Against a dead server that is a
// connection attempt, and a ten second wait, for every report of every kit. The
// link remembers the failure instead: events that arrive while it is backing off
// are counted as lost and dropped without a dial, and the next attempt is made
// when the wait is over. CoT is perishable, so there is nothing to gain by
// queueing a position for a server that was down when it was true.

const (
	// linkQueue bounds what one upstream may have waiting. A position is small
	// and goes stale in two minutes; a queue deeper than this is a server that is
	// not keeping up, and enqueue then loses the oldest event to make room.
	linkQueue = 256

	// linkUrgentQueue is the same for emergencies, which are rare: a queue of
	// them this deep is a server that has stopped, not an incident.
	linkUrgentQueue = 32

	// linkWriteTimeout is how long one write may take before the connection is
	// treated as gone.
	linkWriteTimeout = 10 * time.Second

	// The redial wait after a failure: doubled each time, between these bounds.
	linkBackoffMin = time.Second
	linkBackoffMax = 5 * time.Minute
)

// A connection silent for linkPingAfter is pinged, checked every linkPingCheck. A
// minute: well inside what a NAT or a server's own idle timer allows, and rare
// enough to cost a server nothing. Variables only so a test need not wait a
// minute; nothing else assigns them.
var (
	linkPingAfter = time.Minute
	linkPingCheck = 15 * time.Second
)

// errBackingOff is why an event was dropped without a dial.
var errBackingOff = errors.New("takhosted: not redialling yet")

// job is one event bound for one upstream.
type job struct {
	// up is the upstream as it was resolved for THIS event, so a certificate the
	// tenant has just replaced is the one the next dial presents.
	up      *takfront.Tenant
	payload []byte
}

// link owns one connection to one upstream of one tenant.
type link struct {
	f        *Forwarder
	key      string
	tenantID string
	r        *run
	q        chan job
	urgent   chan job      // emergencies; emptied before q
	wake     chan struct{} // connect now, with nothing to send
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	conn      net.Conn
	up        *takfront.Tenant // this upstream as it was last resolved
	last      time.Time        // the last event written, or when the link was made
	lastWrite time.Time        // the last write of any kind, for pacing keepalives
	fails     int
	nextDial  time.Time
}

func newLink(r *run, f *Forwarder, key, tenantID string) *link {
	ctx, cancel := context.WithCancel(r.ctx)
	return &link{
		f: f, r: r, key: key, tenantID: tenantID,
		q:      make(chan job, linkQueue),
		urgent: make(chan job, linkUrgentQueue),
		wake:   make(chan struct{}, 1),
		ctx:    ctx, cancel: cancel,
		last: time.Now(),
	}
}

// setUp records this upstream as it was just resolved, so a dial made with
// nothing to send presents the certificate the tenant has now.
func (l *link) setUp(up *takfront.Tenant) {
	l.mu.Lock()
	l.up = up
	l.mu.Unlock()
}

// loop writes what is queued until the link is closed.
func (l *link) loop() {
	defer l.closeConn()
	tick := time.NewTicker(linkPingCheck)
	defer tick.Stop()
	for {
		// An emergency that is waiting goes before anything else that is.
		select {
		case j := <-l.urgent:
			l.deliver(j)
			continue
		default:
		}
		select {
		case <-l.ctx.Done():
			return
		case j := <-l.urgent:
			l.deliver(j)
		case j := <-l.q:
			l.deliver(j)
		case <-l.wake:
			l.connect()
		case <-tick.C:
			l.keepalive()
		}
	}
}

// connect opens the connection with nothing to send, so that what the tenant's
// server has to say can be heard while the tenant's own clients are quiet.
func (l *link) connect() {
	l.mu.Lock()
	up := l.up
	l.mu.Unlock()
	if up == nil {
		return
	}
	if _, err := l.connFor(up); err != nil && !errors.Is(err, errBackingOff) && l.ctx.Err() == nil {
		l.f.log.Warn("takhosted: could not reach a tenant's TAK server",
			"tenant", l.tenantID, "upstream", up.Upstream, "kind", up.Label, "error", err)
	}
}

// keepalive writes a ping on a connection that has been silent for
// linkPingAfter. It is how a peer that has gone without closing is noticed: the
// reader would wait on it for ever, and the write fails instead.
func (l *link) keepalive() {
	l.mu.Lock()
	conn, idle := l.conn, time.Since(l.lastWrite)
	l.mu.Unlock()
	if conn == nil || idle < linkPingAfter {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
	if _, err := conn.Write(hubPing(time.Now())); err != nil {
		l.dropConn(conn)
		return
	}
	l.mu.Lock()
	l.lastWrite = time.Now()
	l.mu.Unlock()
}

// deliver writes one event, dialling on first use and redialling once if the held
// connection has gone.
func (l *link) deliver(j job) {
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := l.connFor(j.up)
		if err != nil {
			if !errors.Is(err, errBackingOff) && l.ctx.Err() == nil {
				l.f.log.Warn("takhosted: forwarding a position failed",
					"tenant", l.tenantID, "upstream", j.up.Upstream, "kind", j.up.Label, "error", err)
			}
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
		if _, err := conn.Write(j.payload); err != nil {
			// The upstream went away: drop it and try once more with a fresh one.
			l.closeConn()
			if attempt == 1 {
				forwardsFailed.WithLabelValues(kindOf(j.up), reasonWrite).Inc()
				l.failed()
				if l.ctx.Err() == nil {
					l.f.log.Warn("takhosted: forwarding a position failed",
						"tenant", l.tenantID, "upstream", j.up.Upstream, "kind", j.up.Label,
						"error", fmt.Errorf("write to %s: %w", j.up.Upstream, err))
				}
			}
			continue
		}
		l.mu.Lock()
		l.last = time.Now()
		l.lastWrite = l.last
		l.mu.Unlock()
		return
	}
}

// connFor returns the held connection or dials one.
func (l *link) connFor(up *takfront.Tenant) (net.Conn, error) {
	l.mu.Lock()
	if l.conn != nil {
		c := l.conn
		l.mu.Unlock()
		return c, nil
	}
	wait := time.Until(l.nextDial)
	l.mu.Unlock()
	if wait > 0 {
		// Counted, because the event is lost; not logged, because the dial that
		// failed already said why and this happens once per event.
		forwardsFailed.WithLabelValues(kindOf(up), reasonDial).Inc()
		return nil, errBackingOff
	}

	conn, err := l.f.dial(l.ctx, up)
	if err != nil {
		forwardsFailed.WithLabelValues(kindOf(up), reasonDial).Inc()
		l.failed()
		return nil, fmt.Errorf("dial %s: %w", up.Upstream, err)
	}
	// A successful dial is not a working connection (MESHSAT-1066). Under TLS 1.3
	// the server receives our certificate after it has finished its own handshake,
	// so it refuses us on a connection we already believe is open. Find that out
	// HERE: before the connection is kept, and before the log below claims it
	// opened.
	first, why := upstreamRejected(conn)
	if why != nil {
		_ = conn.Close()
		forwardsFailed.WithLabelValues(kindOf(up), reasonRefused).Inc()
		l.failed()
		return nil, fmt.Errorf("%s refused this Hub: %w", up.Upstream, why)
	}
	// Say who this is before anything else is written; see hello.go for why a
	// server needs that before it will send anything back.
	_ = conn.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
	if _, err := conn.Write(hubHello(l.tenantID, time.Now())); err != nil {
		_ = conn.Close()
		forwardsFailed.WithLabelValues(kindOf(up), reasonWrite).Inc()
		l.failed()
		return nil, fmt.Errorf("write to %s: %w", up.Upstream, err)
	}

	l.mu.Lock()
	if l.ctx.Err() != nil {
		// Closed while dialling: nobody would ever close this connection.
		l.mu.Unlock()
		_ = conn.Close()
		return nil, l.ctx.Err()
	}
	l.conn = conn
	l.fails = 0
	l.nextDial = time.Time{}
	l.lastWrite = time.Now()
	l.mu.Unlock()
	// The tenant and the address, not the key: a reader wants to know whose server
	// this is and where it is, and the key is an implementation detail.
	l.f.log.Info("takhosted: opened a CoT connection to a tenant's TAK server",
		"tenant", up.TenantID, "upstream", up.Upstream, "kind", up.Label)

	// What the server sends back is read by a goroutine of its own, for as long
	// as this connection lives. Counted in the run's wait group; adding to it
	// here is safe because this runs on the link's own goroutine, which the group
	// is already waiting for.
	l.r.wg.Add(1)
	go func() {
		defer l.r.wg.Done()
		l.read(conn, up, first)
	}()
	return conn, nil
}

// read takes what the server sends on one connection, until that connection
// ends. first is what the dial probe had already read.
//
// A plain conn.Read in a loop with no deadline (rule 15). It is unblocked by the
// connection being closed, which is what closeConn, close and a failed write all
// do.
func (l *link) read(conn net.Conn, up *takfront.Tenant, first []byte) {
	defer l.dropConn(conn)
	framer := tak.NewFramer(tak.UpstreamLimits.MaxBytes)
	take := func(p []byte) bool {
		frames, err := framer.Feed(p)
		for _, frame := range frames {
			l.f.onUpstream(l.r, l.tenantID, up, frame)
		}
		if err != nil {
			// The stream has lost its meaning: not CoT XML, or an event with no
			// end. Closing is the only way back to a known state.
			forwardsFailed.WithLabelValues(kindOf(up), reasonStream).Inc()
			if l.ctx.Err() == nil {
				l.f.log.Warn("takhosted: closing a TAK server connection whose stream could not be read",
					"tenant", l.tenantID, "upstream", up.Upstream, "kind", up.Label, "error", err)
			}
			return false
		}
		return true
	}
	if len(first) > 0 && !take(first) {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 && !take(buf[:n]) {
			return
		}
		if err != nil {
			return
		}
	}
}

// dropConn closes one connection and forgets it if it is still the link's.
func (l *link) dropConn(conn net.Conn) {
	l.mu.Lock()
	if l.conn == conn {
		l.conn = nil
	}
	l.mu.Unlock()
	_ = conn.Close()
}

// failed records a failure and sets when the next dial may be made.
func (l *link) failed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails++
	l.nextDial = time.Now().Add(backoffFor(l.fails, time.Now()))
}

// backoffFor is the wait after the nth consecutive failure: one second doubled
// each time up to five minutes, less up to a fifth so that links which failed
// together do not redial together.
//
// The spread comes from the clock rather than a random source. It needs to differ
// between links, not to be unpredictable, and this keeps a second source of
// randomness out of a package that has no other use for one.
func backoffFor(fails int, now time.Time) time.Duration {
	d := linkBackoffMin
	for i := 1; i < fails && d < linkBackoffMax; i++ {
		d *= 2
	}
	if d > linkBackoffMax {
		d = linkBackoffMax
	}
	spread := int64(d) / 5
	if spread > 0 {
		d -= time.Duration(now.UnixNano() % spread)
	}
	return d
}

func (l *link) closeConn() {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// close stops the link. The connection is closed as well as the context
// cancelled, because a write blocked on a peer that has stopped reading does not
// look at a context.
func (l *link) close() {
	l.cancel()
	l.closeConn()
}

// idleSince reports when the link last did anything useful.
func (l *link) idleSince() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}
