package relay

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// Options tune one Relay. Zero values take the defaults below.
type Options struct {
	// PingInterval is how often the Hub pings a silent socket (30 s).
	PingInterval time.Duration
	// PongWait is how long a socket may stay silent before it is closed (60 s).
	PongWait time.Duration
	// WriteTimeout bounds one frame write (5 s).
	WriteTimeout time.Duration
	// FramesPerMinute is the per-client budget (100). 0 disables it.
	FramesPerMinute int
	// MaxFrame is the largest frame accepted from either end (64 KiB).
	MaxFrame int64
	// Upgrader checks the Origin header; nil accepts non-browser clients only.
	Upgrader *websocket.Upgrader
}

func (o Options) withDefaults() Options {
	if o.PingInterval <= 0 {
		o.PingInterval = 30 * time.Second
	}
	if o.PongWait <= 0 {
		o.PongWait = 60 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 5 * time.Second
	}
	if o.FramesPerMinute == 0 {
		o.FramesPerMinute = 100
	}
	if o.MaxFrame <= 0 {
		o.MaxFrame = 64 << 10
	}
	if o.Upgrader == nil {
		o.Upgrader = &websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	}
	return o
}

// Close codes on the wire, so the other end can tell why.
const (
	// CloseBudget: the client spent its minute's frames. Reconnect next minute.
	CloseBudget = websocket.ClosePolicyViolation // 1008
	// CloseSuperseded: the same identity opened a newer socket on this replica.
	CloseSuperseded = websocket.CloseGoingAway // 1001
	// CloseFrame: a frame the Hub could not accept (not binary, bad envelope).
	CloseFrame = websocket.CloseUnsupportedData // 1003
)

// Relay holds this replica's sessions and joins them over the bus.
type Relay struct {
	bus    bus.MessageBus
	budget Budget
	opt    Options

	mu      sync.Mutex
	bridges map[string]*session // tenant/bridge
	clients map[string]*session // tenant/bridge/client
}

// New returns a relay over b with the given budget (nil means a memory one).
func New(b bus.MessageBus, budget Budget, opt Options) *Relay {
	if budget == nil {
		budget = NewMemoryBudget()
	}
	return &Relay{bus: b, budget: budget, opt: opt.withDefaults(), bridges: map[string]*session{}, clients: map[string]*session{}}
}

// Start subscribes this replica to the relay topics of every tenant. Plain
// Subscribe, deliberately not a queue group: the replica that holds a socket
// must be the one that receives, and only it knows that it does.
func (r *Relay) Start() error {
	for _, f := range hubmqtt.RelayFilters("up") {
		if err := r.bus.Subscribe(f, 0, r.onUp); err != nil {
			return err
		}
	}
	for _, f := range hubmqtt.RelayFilters("down") {
		if err := r.bus.Subscribe(f, 0, r.onDown); err != nil {
			return err
		}
	}
	return nil
}

// Admit spends one unit of the client's budget for a connect attempt. It is
// asked BEFORE the upgrade so an exhausted client gets a 429 it can read
// rather than a socket that closes on it.
func (r *Relay) Admit(tenantID, clientID string) bool {
	ok := r.budget.Allow(budgetKey(tenantID, clientID), r.opt.FramesPerMinute)
	if !ok {
		metrics.RelayRejected.WithLabelValues("budget").Inc()
	}
	return ok
}

// Sessions reports how many sockets of a role this replica holds.
func (r *Relay) Sessions(role string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if role == "bridge" {
		return len(r.bridges)
	}
	return len(r.clients)
}

func budgetKey(tenantID, clientID string) string { return tenantID + ":" + clientID }
func bridgeKey(tenantID, bridgeID string) string { return tenantID + "/" + bridgeID }
func clientKey(tenantID, bridgeID, clientID string) string {
	return tenantID + "/" + bridgeID + "/" + clientID
}

// session is one socket, either end.
type session struct {
	conn   *websocket.Conn
	role   string
	tenant string
	bridge string
	client string // empty for a bridge session

	wmu       sync.Mutex // gorilla permits one concurrent writer
	done      chan struct{}
	closeOnce sync.Once
	opt       Options
}

func (s *session) write(mt int, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.opt.WriteTimeout))
	return s.conn.WriteMessage(mt, data)
}

// close sends a close frame with a reason the other end can log, then closes.
func (s *session) close(code int, reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wmu.Lock()
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.opt.WriteTimeout))
		_ = s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
		s.wmu.Unlock()
		_ = s.conn.Close()
	})
}

// pinger keeps a silent socket alive and lets the read deadline catch a dead
// one: the pong handler extends the deadline, so a peer that stops answering
// is closed PongWait after its last sign of life.
func (s *session) pinger() {
	t := time.NewTicker(s.opt.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.wmu.Lock()
			_ = s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.opt.WriteTimeout))
			s.wmu.Unlock()
		}
	}
}

func (r *Relay) upgrade(w http.ResponseWriter, req *http.Request, role, tenant, bridge, client string) (*session, bool) {
	conn, err := r.opt.Upgrader.Upgrade(w, req, nil)
	if err != nil {
		// Upgrade has already written its own response.
		metrics.RelayRejected.WithLabelValues("upgrade").Inc()
		return nil, false
	}
	s := &session{conn: conn, role: role, tenant: tenant, bridge: bridge, client: client, done: make(chan struct{}), opt: r.opt}
	conn.SetReadLimit(r.opt.MaxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(r.opt.PongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(r.opt.PongWait)) })
	return s, true
}

// ServeBridge is the bridge end: one socket, every client of that bridge
// multiplexed through the envelope. The caller has authenticated the bridge
// and resolved its tenant; nothing here trusts the request for either.
func (r *Relay) ServeBridge(w http.ResponseWriter, req *http.Request, tenantID, bridgeID string) {
	s, ok := r.upgrade(w, req, "bridge", tenantID, bridgeID, "")
	if !ok {
		return
	}
	key := bridgeKey(tenantID, bridgeID)
	r.mu.Lock()
	if old := r.bridges[key]; old != nil {
		old.close(CloseSuperseded, "superseded by a newer bridge socket")
	}
	r.bridges[key] = s
	r.mu.Unlock()
	metrics.RelaySessions.WithLabelValues("bridge").Inc()
	slog.Info("relay: bridge serving", "tenant", tenantID, "bridge", bridgeID, "remote", s.conn.RemoteAddr())

	go s.pinger()
	defer func() {
		r.mu.Lock()
		if r.bridges[key] == s {
			delete(r.bridges, key)
		}
		r.mu.Unlock()
		metrics.RelaySessions.WithLabelValues("bridge").Dec()
		s.close(websocket.CloseNormalClosure, "")
		slog.Info("relay: bridge gone", "tenant", tenantID, "bridge", bridgeID)
	}()

	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(r.opt.PongWait))
		if mt != websocket.BinaryMessage {
			metrics.RelayRejected.WithLabelValues("frame").Inc()
			s.close(CloseFrame, "binary frames only")
			return
		}
		clientID, payload, err := DecodeEnvelope(data)
		if err != nil {
			metrics.RelayRejected.WithLabelValues("envelope").Inc()
			s.close(CloseFrame, err.Error())
			return
		}
		metrics.RelayFrames.WithLabelValues("down").Inc()
		if err := r.bus.Publish(hubmqtt.RelayTopic(tenantID, bridgeID, clientID, "down"), 0, false, payload); err != nil {
			slog.Warn("relay: publish down failed", "tenant", tenantID, "bridge", bridgeID, "error", err)
		}
	}
}

// ServeClient is the client end: one socket, one tunnel to one bridge. The
// caller has authenticated the client, checked that the bridge is the same
// tenant's, and called Admit.
func (r *Relay) ServeClient(w http.ResponseWriter, req *http.Request, tenantID, bridgeID, clientID string) {
	s, ok := r.upgrade(w, req, "client", tenantID, bridgeID, clientID)
	if !ok {
		return
	}
	key := clientKey(tenantID, bridgeID, clientID)
	r.mu.Lock()
	if old := r.clients[key]; old != nil {
		old.close(CloseSuperseded, "superseded by a newer client socket")
	}
	r.clients[key] = s
	r.mu.Unlock()
	metrics.RelaySessions.WithLabelValues("client").Inc()
	slog.Info("relay: client connected", "tenant", tenantID, "bridge", bridgeID, "client", clientID, "remote", s.conn.RemoteAddr())

	go s.pinger()
	defer func() {
		r.mu.Lock()
		if r.clients[key] == s {
			delete(r.clients, key)
		}
		r.mu.Unlock()
		metrics.RelaySessions.WithLabelValues("client").Dec()
		s.close(websocket.CloseNormalClosure, "")
		slog.Info("relay: client gone", "tenant", tenantID, "bridge", bridgeID, "client", clientID)
	}()

	bk := budgetKey(tenantID, clientID)
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(r.opt.PongWait))
		if mt != websocket.BinaryMessage {
			metrics.RelayRejected.WithLabelValues("frame").Inc()
			s.close(CloseFrame, "binary frames only")
			return
		}
		if !r.budget.Allow(bk, r.opt.FramesPerMinute) {
			metrics.RelayRejected.WithLabelValues("budget").Inc()
			s.close(CloseBudget, "frame budget exhausted for this minute")
			return
		}
		metrics.RelayFrames.WithLabelValues("up").Inc()
		if err := r.bus.Publish(hubmqtt.RelayTopic(tenantID, bridgeID, clientID, "up"), 0, false, data); err != nil {
			slog.Warn("relay: publish up failed", "tenant", tenantID, "bridge", bridgeID, "client", clientID, "error", err)
		}
	}
}

// onUp delivers a client's frame to the bridge socket, if this replica holds it.
func (r *Relay) onUp(topic string, payload []byte) {
	tenantID, bridgeID, clientID, _, ok := hubmqtt.ParseRelayTopic(topic)
	if !ok {
		return
	}
	r.mu.Lock()
	s := r.bridges[bridgeKey(tenantID, bridgeID)]
	r.mu.Unlock()
	if s == nil {
		return
	}
	frame, err := EncodeEnvelope(clientID, payload)
	if err != nil {
		return
	}
	if err := s.write(websocket.BinaryMessage, frame); err != nil {
		s.close(websocket.CloseAbnormalClosure, "write failed")
	}
}

// onDown delivers a bridge's reply to the client socket, if this replica holds it.
func (r *Relay) onDown(topic string, payload []byte) {
	tenantID, bridgeID, clientID, _, ok := hubmqtt.ParseRelayTopic(topic)
	if !ok {
		return
	}
	r.mu.Lock()
	s := r.clients[clientKey(tenantID, bridgeID, clientID)]
	r.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.write(websocket.BinaryMessage, payload); err != nil {
		s.close(websocket.CloseAbnormalClosure, "write failed")
	}
}
