// Package matrixbridge lets a tenant's dispatchers write to their crews from
// the Matrix rooms they already work in, and read the answers there
// (MESHSAT-1736, Mode A).
//
// The tenant gives the Hub an ordinary bot account on its own homeserver and
// the rooms it may read (Settings, Integrations, "matrix"). In those rooms a
// message that starts with @ and a crew's callsign is handed to
// dispatcher-to-crew chat (internal/takchat) as written by that Matrix user;
// nothing else said in a room leaves it, so a room is never mirrored onto a
// radio. The crew's answer, which takchat stores addressed to that user's
// operator id, is posted back into the room.
//
// # Replicas
//
// Reading runs on the leader alone (Run, a singleton). Where the bot left off
// (the homeserver's next_batch) is stored after every batch, so a new leader
// carries on; a message read twice across a handover is claimed once by its
// event id (store.ClaimOnce), so it reaches the radio once. Posting an answer
// runs on whichever replica stored it (OnReply, takchat's reply hook), claimed
// once per answer.
//
// # Encrypted rooms
//
// Not supported. The bot cannot read an encrypted room, and it must never post
// plaintext into one, so a room that is or becomes encrypted is skipped for
// reading and for answers alike.
package matrixbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takchat"
)

const (
	// OperatorPrefix starts the operator id of every Matrix user: takchat
	// writes as meshsat-hub-op-mx<16 hex>.
	OperatorPrefix = "mx"
	// roomPerHour is how many messages one room may send to crews an hour, on
	// top of takchat's ceilings per crew and per tenant.
	roomPerHour = 30
	// noticesPerMinute bounds the bot's own lines in one room.
	noticesPerMinute = 6
	// crewFresh matches takchat's: a crew unseen for a day cannot be written to.
	crewFresh = 24 * time.Hour
	// answerQueue bounds the answers waiting to be posted on one replica.
	answerQueue = 128
	// timelineLimit is how many events one sync may carry per room.
	timelineLimit = 50
	maxBackoff    = 5 * time.Minute
)

var (
	messagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_matrix_messages_total",
		Help: "Messages the Matrix bridge read in a dispatcher room, by what it did with them (MESHSAT-1736).",
	}, []string{"result"})
	answersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_matrix_answers_total",
		Help: "Crews' answers to a Matrix user, by whether they reached the room (MESHSAT-1736).",
	}, []string{"result"})
	statusesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_matrix_statuses_total",
		Help: "Kits' words on a Matrix user's message (not delivered, delivered), by whether they reached the room (MESHSAT-1746).",
	}, []string{"result"})
	syncFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_matrix_sync_failures_total",
		Help: "Reads from a tenant's homeserver that failed; the bot retries with backoff (MESHSAT-1736).",
	})
)

func init() {
	for _, r := range []string{"sent", "refused", "unknown_crew", "command", "not_addressed", "sender_not_allowed", "encrypted_room"} {
		messagesTotal.WithLabelValues(r)
	}
	for _, r := range []string{"posted", "failed", "dropped"} {
		answersTotal.WithLabelValues(r)
		statusesTotal.WithLabelValues(r)
	}
}

// Accounts is what the bridge needs of internal/integrations.
type Accounts interface {
	TenantsWith(ctx context.Context, provider string) ([]string, error)
	ForTenant(ctx context.Context, tenantID, provider string) (*integrations.Account, error)
}

// Chat is what the bridge needs of internal/takchat.
type Chat interface {
	Enabled() bool
	Send(ctx context.Context, req takchat.SendRequest) (*store.TAKChatMessage, bool, error)
}

// Service reads the tenants' dispatcher rooms and posts the crews' answers.
type Service struct {
	store    store.Store
	accounts Accounts
	chat     Chat
	audit    *audit.Service
	log      *slog.Logger
	enabled  bool
	now      func() time.Time
	answers  chan *store.TAKChatMessage
	statuses chan *store.TAKChatMessage

	mu      sync.Mutex
	workers map[string]*worker
}

// New returns a Service. enabled is HUB_MATRIX_ENABLED; with takchat off the
// bridge is off too.
func New(s store.Store, accounts Accounts, chat Chat, a *audit.Service, enabled bool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: s, accounts: accounts, chat: chat, audit: a, log: log,
		enabled: enabled && chat != nil && chat.Enabled(), now: time.Now,
		answers: make(chan *store.TAKChatMessage, answerQueue), statuses: make(chan *store.TAKChatMessage, answerQueue),
		workers: map[string]*worker{}}
}

// Enabled reports whether the bridge is switched on.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// --- configuration -----------------------------------------------------------

type config struct {
	homeserver string
	token      string
	rooms      []string
	roomSet    map[string]bool
	senders    []string
	fp         string
}

// parseConfig reads a tenant's Matrix account. A room is a room id (!x:server);
// a sender a user id (@x:server) or a whole homeserver (:server).
func parseConfig(a *integrations.Account) (*config, error) {
	c := &config{homeserver: strings.TrimSpace(a.Get("homeserver_url")), token: strings.TrimSpace(a.Get("access_token")),
		roomSet: map[string]bool{}}
	if c.homeserver == "" || c.token == "" {
		return nil, errors.New("the homeserver URL and the bot's access token are both needed")
	}
	for _, r := range splitList(a.Get("rooms")) {
		if !strings.HasPrefix(r, "!") || !strings.Contains(r, ":") {
			return nil, fmt.Errorf("%q is not a room id; it looks like !AbCdEfGh:example.org", clip(r, 60))
		}
		if !c.roomSet[r] {
			c.roomSet[r] = true
			c.rooms = append(c.rooms, r)
		}
	}
	if len(c.rooms) == 0 {
		return nil, errors.New("no room is listed")
	}
	for _, s := range splitList(a.Get("senders")) {
		if (!strings.HasPrefix(s, "@") && !strings.HasPrefix(s, ":")) || !strings.Contains(s, ":") {
			return nil, fmt.Errorf("%q is neither a user id (@anna:example.org) nor a homeserver (:example.org)", clip(s, 60))
		}
		c.senders = append(c.senders, s)
	}
	sum := sha256.Sum256([]byte(c.homeserver + "\x00" + c.token + "\x00" + strings.Join(c.rooms, "\x00") + "\x00" + strings.Join(c.senders, "\x00")))
	c.fp = hex.EncodeToString(sum[:])
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// senderAllowed applies the senders list; empty lets everyone in the room.
func (c *config) senderAllowed(user string) bool {
	if len(c.senders) == 0 {
		return true
	}
	for _, s := range c.senders {
		if s == user || (strings.HasPrefix(s, ":") && strings.HasSuffix(user, s)) {
			return true
		}
	}
	return false
}

// --- the readers, on the leader ----------------------------------------------

// Run keeps one reader per tenant with a Matrix account until ctx ends,
// reconciling every tick. A leader singleton: ctx ends when leadership does.
func (s *Service) Run(ctx context.Context, tick time.Duration) {
	if !s.Enabled() {
		return
	}
	s.reconcile(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	want := map[string]*config{}
	tenants, err := s.accounts.TenantsWith(ctx, integrations.ProviderMatrix)
	if err != nil {
		s.log.Warn("matrix: listing the tenants with a Matrix account failed", "error", err)
		return // keep what runs rather than stop it on a database blip
	}
	for _, tid := range tenants {
		acct, err := s.accounts.ForTenant(ctx, tid, integrations.ProviderMatrix)
		if err != nil || acct == nil || acct.Platform {
			continue
		}
		cfg, err := parseConfig(acct)
		if err != nil {
			s.log.Warn("matrix: a tenant's Matrix account cannot be used", "tenant", tid, "error", err)
			continue
		}
		want[tid] = cfg
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for tid, w := range s.workers {
		if c, ok := want[tid]; !ok || c.fp != w.cfg.fp {
			w.stop()
			delete(s.workers, tid)
		}
	}
	for tid, c := range want {
		if _, ok := s.workers[tid]; ok {
			continue
		}
		s.workers[tid] = s.startWorker(ctx, tid, c)
	}
}

func (s *Service) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tid, w := range s.workers {
		w.stop()
		delete(s.workers, tid)
	}
}

type worker struct {
	s      *Service
	tenant string
	cfg    *config
	cancel context.CancelFunc
	done   chan struct{}

	bot       string
	encrypted map[string]bool
	names     map[string]string
}

func (s *Service) startWorker(parent context.Context, tid string, c *config) *worker {
	ctx, cancel := context.WithCancel(parent)
	w := &worker{s: s, tenant: tid, cfg: c, cancel: cancel, done: make(chan struct{}),
		encrypted: map[string]bool{}, names: map[string]string{}}
	go w.run(ctx)
	return w
}

func (w *worker) stop() {
	w.cancel()
	<-w.done
}

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	var backoff time.Duration
	for ctx.Err() == nil {
		healthy, err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		syncFailures.Inc()
		if healthy {
			backoff = 0
		}
		backoff = min(max(2*backoff, 5*time.Second), maxBackoff)
		wait := backoff
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > wait {
			wait = min(ae.RetryAfter, maxBackoff)
		}
		w.s.log.Warn("matrix: reading a tenant's rooms stopped, retrying", "tenant", w.tenant, "in", wait, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// session signs in, joins the rooms and reads until an error. healthy is
// whether at least one read succeeded, which resets the backoff.
func (w *worker) session(ctx context.Context) (healthy bool, err error) {
	c := newClient(w.cfg.homeserver, w.cfg.token)
	w.bot, err = c.whoami(ctx)
	if err != nil {
		return false, err
	}
	for _, room := range w.cfg.rooms {
		if err := c.join(ctx, room); err != nil {
			w.s.log.Warn("matrix: the bot cannot join a room; invite it first", "tenant", w.tenant, "room", room, "error", err)
		}
	}
	since := ""
	st, err := w.s.store.GetMatrixSync(ctx, w.tenant)
	if err != nil {
		return false, err
	}
	if st != nil && st.BotUserID == w.bot {
		since = st.NextBatch
	}
	if since == "" {
		// A first start reads from now: what was said before the bot was set
		// up was never meant for a radio.
		r, err := c.sync(ctx, "", w.filter(1), 0)
		if err != nil {
			return false, err
		}
		since = r.NextBatch
		if err := w.s.store.SaveMatrixSync(ctx, &store.MatrixSync{TenantID: w.tenant, BotUserID: w.bot, NextBatch: since}); err != nil {
			return false, err
		}
		w.s.log.Info("matrix: a tenant's bot starts reading from now", "tenant", w.tenant, "bot", w.bot)
	}
	for {
		r, err := c.sync(ctx, since, w.filter(timelineLimit), syncTimeout)
		if err != nil {
			return healthy, err
		}
		healthy = true
		rooms := make([]string, 0, len(r.Rooms.Join))
		for id := range r.Rooms.Join {
			rooms = append(rooms, id)
		}
		sort.Strings(rooms)
		for _, room := range rooms {
			if !w.cfg.roomSet[room] {
				continue
			}
			tl := r.Rooms.Join[room].Timeline
			if tl.Limited {
				w.s.log.Warn("matrix: a room had more than one batch can carry; older messages were skipped", "tenant", w.tenant, "room", room)
			}
			for _, ev := range tl.Events {
				w.handle(ctx, c, room, ev)
			}
		}
		since = r.NextBatch
		if err := w.s.store.SaveMatrixSync(ctx, &store.MatrixSync{TenantID: w.tenant, BotUserID: w.bot, NextBatch: since}); err != nil {
			w.s.log.Warn("matrix: could not store where the bot left off", "tenant", w.tenant, "error", err)
		}
	}
}

// filter asks the homeserver for the listed rooms' messages and nothing else.
func (w *worker) filter(limit int) string {
	none := map[string]any{"not_types": []string{"*"}}
	b, _ := json.Marshal(map[string]any{
		"presence":     none,
		"account_data": none,
		"room": map[string]any{
			"rooms":        w.cfg.rooms,
			"timeline":     map[string]any{"types": []string{"m.room.message", "m.room.encryption"}, "limit": limit},
			"state":        none,
			"ephemeral":    none,
			"account_data": none,
		},
	})
	return string(b)
}

type messageContent struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	RelatesTo *struct {
		RelType string `json:"rel_type"`
	} `json:"m.relates_to"`
}

// handle decides what one event in a listed room is.
func (w *worker) handle(ctx context.Context, c *client, room string, ev event) {
	if ev.Type == "m.room.encryption" {
		w.encrypted[room] = true
		w.s.log.Warn("matrix: a room was switched to encryption; the bridge no longer reads or answers there", "tenant", w.tenant, "room", room)
		return
	}
	if ev.Type != "m.room.message" || ev.Sender == w.bot || ev.EventID == "" {
		return
	}
	var mc messageContent
	if json.Unmarshal(ev.Content, &mc) != nil || mc.MsgType != "m.text" {
		return
	}
	if mc.RelatesTo != nil && mc.RelatesTo.RelType == "m.replace" {
		return // an edit: the radio already has the first version or nothing
	}
	body := strings.TrimSpace(stripReplyFallback(mc.Body))
	if body != "!crews" && !strings.HasPrefix(body, "@") {
		messagesTotal.WithLabelValues("not_addressed").Inc()
		return
	}
	if w.isEncrypted(ctx, c, room) {
		messagesTotal.WithLabelValues("encrypted_room").Inc()
		return
	}
	if !w.cfg.senderAllowed(ev.Sender) {
		messagesTotal.WithLabelValues("sender_not_allowed").Inc()
		return
	}
	// A handover can read a batch twice; the event id makes it one message.
	if ok, err := w.s.store.ClaimOnce(ctx, "matrix:event:"+w.tenant+":"+ev.EventID); err != nil || !ok {
		return
	}
	now := w.s.now()
	crews, err := w.s.store.ListTAKContacts(ctx, w.tenant, now.Add(-crewFresh))
	if err != nil {
		w.s.log.Warn("matrix: could not list the crews", "tenant", w.tenant, "error", err)
		return
	}
	if body == "!crews" {
		messagesTotal.WithLabelValues("command").Inc()
		w.say(ctx, c, room, ev.EventID, crewList(crews))
		return
	}
	crew, text := matchCrew(crews, body[1:])
	if crew == nil {
		name := firstToken(body[1:])
		if name == "" || strings.Contains(name, ":") {
			messagesTotal.WithLabelValues("not_addressed").Inc()
			return // a Matrix user id, or a bare @: talk between people
		}
		messagesTotal.WithLabelValues("unknown_crew").Inc()
		w.say(ctx, c, room, ev.EventID, fmt.Sprintf("No crew called %s has been seen in the last day. Send !crews for the list.", clip(name, 40)))
		return
	}
	w.send(ctx, c, room, ev, crew, text, now)
}

func (w *worker) send(ctx context.Context, c *client, room string, ev event, crew *store.TAKContact, text string, now time.Time) {
	refuse := func(why string) {
		messagesTotal.WithLabelValues("refused").Inc()
		w.say(ctx, c, room, ev.EventID, why)
	}
	if ok, err := w.s.store.TakePersonChatSlot(ctx, w.tenant,
		"mx:room:"+roomKey(room)+":"+now.UTC().Format("2006010215"), roomPerHour); err != nil {
		w.s.log.Warn("matrix: the room's ceiling could not be read", "tenant", w.tenant, "error", err)
		return
	} else if !ok {
		refuse(fmt.Sprintf("This room has written to crews %d times this hour, the most it may. Wait for the next hour.", roomPerHour))
		return
	}
	op := operatorID(w.tenant, room, ev.Sender)
	name := w.displayName(ctx, c, room, ev.Sender)
	// Stored before the send: an answer can come back within seconds.
	if err := w.s.store.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: w.tenant, UserID: op, RoomID: room,
		MatrixUserID: ev.Sender, DisplayName: name, LastUsedAt: now}); err != nil {
		w.s.log.Warn("matrix: could not record who wrote, not sending", "tenant", w.tenant, "error", err)
		return
	}
	msg, created, err := w.s.chat.Send(ctx, takchat.SendRequest{TenantID: w.tenant, CrewUID: crew.UID, UserID: op,
		Name: name, Text: text, ClientRef: "matrix:" + ev.EventID})
	switch {
	case errors.Is(err, takchat.ErrDisabled):
		refuse("Writing to crews is switched off on this Hub.")
		return
	case errors.Is(err, takchat.ErrEmpty):
		refuse("Nothing to send after " + crew.Callsign + ".")
		return
	case errors.Is(err, takchat.ErrTooLong):
		refuse(fmt.Sprintf("Too long for a radio: at most %d bytes.", takchat.MaxBytes))
		return
	case errors.Is(err, takchat.ErrStale), errors.Is(err, takchat.ErrNoCrew):
		refuse(crew.Callsign + " has not been seen for a day.")
		return
	case errors.Is(err, takchat.ErrUnregistered):
		refuse("The kit or phone that carries " + crew.Callsign + " is no longer registered.")
		return
	case errors.Is(err, takchat.ErrNoBearer):
		refuse(crew.Callsign + "'s kit is off MQTT and nothing else reaches it: " + strings.TrimPrefix(err.Error(), takchat.ErrNoBearer.Error()+" "))
		return
	case errors.Is(err, takchat.ErrFrameTooLong):
		refuse("Too long for the way " + crew.Callsign + "'s kit is reachable: " + strings.TrimPrefix(err.Error(), takchat.ErrFrameTooLong.Error()+": "))
		return
	case errors.Is(err, takchat.ErrLimited):
		refuse("Too many messages to " + crew.Callsign + " just now. Wait a few seconds.")
		return
	case errors.Is(err, takchat.ErrPublish):
		refuse("The message could not be handed to " + crew.Callsign + "'s kit. Try again.")
		return
	case err != nil:
		w.s.log.Warn("matrix: a message to a crew could not be sent", "tenant", w.tenant, "error", err)
		refuse("The message could not be sent. Try again.")
		return
	}
	if !created {
		return
	}
	messagesTotal.WithLabelValues("sent").Inc()
	if err := c.send(ctx, room, "m.reaction", "ack-"+ev.EventID, map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": ev.EventID, "key": "✅"},
	}); err != nil {
		w.s.log.Warn("matrix: could not mark a message as sent", "tenant", w.tenant, "error", err)
	}
	// A kit off MQTT got it as a frame (MESHSAT-1744): say so, since it
	// took a satellite credit or an SMS and takes its time.
	switch {
	case takchat.IsSatellite(msg.Bearer):
		w.say(ctx, c, room, ev.EventID+"-bearer", "Sent to "+crew.Callsign+" over satellite: its kit is off MQTT.")
	case msg.Bearer == takchat.BearerSMS:
		w.say(ctx, c, room, ev.EventID+"-bearer", "Sent to "+crew.Callsign+" over SMS: its kit is off MQTT.")
	}
	if w.s.audit != nil {
		// Who wrote to which crew from which room; never the words.
		_ = w.s.audit.Log(ctx, w.tenant, "matrix_chat_sent", ev.Sender,
			"crew "+crew.UID+" via "+msg.BridgeID+" message "+msg.ID+" bearer "+msg.Bearer+" room "+room, "")
	}
}

// say posts one of the bot's own lines, a few a minute per room at most.
func (w *worker) say(ctx context.Context, c *client, room, eventID, text string) {
	if ok, err := w.s.store.TakePersonChatSlot(ctx, w.tenant,
		"mx:notice:"+roomKey(room)+":"+w.s.now().UTC().Format("200601021504"), noticesPerMinute); err != nil || !ok {
		return
	}
	if err := c.notice(ctx, room, "say-"+eventID, text); err != nil {
		w.s.log.Warn("matrix: could not answer in a room", "tenant", w.tenant, "room", room, "error", err)
	}
}

// isEncrypted answers from what the bot has seen, asking the homeserver when
// it has not; a room it cannot tell about counts as encrypted for now.
func (w *worker) isEncrypted(ctx context.Context, c *client, room string) bool {
	if enc, ok := w.encrypted[room]; ok {
		return enc
	}
	enc, err := c.encrypted(ctx, room)
	if err != nil {
		w.s.log.Warn("matrix: could not tell whether a room is encrypted; skipping the message", "tenant", w.tenant, "room", room, "error", err)
		return true
	}
	w.encrypted[room] = enc
	if enc {
		w.s.log.Warn("matrix: a listed room is encrypted, which the bridge does not support yet", "tenant", w.tenant, "room", room)
	}
	return enc
}

func (w *worker) displayName(ctx context.Context, c *client, room, user string) string {
	key := room + "|" + user
	if n, ok := w.names[key]; ok {
		return n
	}
	n := strings.TrimSpace(c.displayName(ctx, room, user))
	if n == "" {
		n = strings.TrimPrefix(user, "@")
		if i := strings.IndexByte(n, ':'); i > 0 {
			n = n[:i]
		}
	}
	if len(w.names) > 1000 {
		w.names = map[string]string{}
	}
	w.names[key] = n
	return n
}

// --- the answers, on every replica -------------------------------------------

// OnReply is takchat's reply hook: it queues a crew's answer to a Matrix user
// for posting. It runs on the sink's goroutine, so it only hands over.
func (s *Service) OnReply(_ context.Context, m *store.TAKChatMessage) {
	if !s.Enabled() || m == nil || m.Direction != "in" || !strings.HasPrefix(m.UserID, OperatorPrefix) {
		return
	}
	cp := *m
	select {
	case s.answers <- &cp:
	default:
		answersTotal.WithLabelValues("dropped").Inc()
		s.log.Warn("matrix: the answer poster is behind, an answer was not posted", "tenant", m.TenantID)
	}
}

// OnStatus is takchat's status hook (MESHSAT-1746): it queues a kit's word
// on a Matrix user's message for the room. It runs on the sink's goroutine,
// so it only hands over. A "queued" says nothing in the room: the ✅ the
// message got when it was handed to the kit already stands for that.
func (s *Service) OnStatus(_ context.Context, m *store.TAKChatMessage) {
	if !s.Enabled() || m == nil || m.Direction != "out" || !strings.HasPrefix(m.UserID, OperatorPrefix) {
		return
	}
	switch m.Status {
	case takchat.StatusDelivered, takchat.StatusFailed, takchat.StatusUndelivered:
	default:
		return
	}
	cp := *m
	select {
	case s.statuses <- &cp:
	default:
		statusesTotal.WithLabelValues("dropped").Inc()
		s.log.Warn("matrix: the status poster is behind, a kit's word was not posted", "tenant", m.TenantID)
	}
}

// Start posts queued answers and statuses until ctx is done. It runs on
// every replica.
func (s *Service) Start(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-s.answers:
				pctx, cancel := context.WithTimeout(ctx, callTimeout)
				s.postAnswer(pctx, m)
				cancel()
			case m := <-s.statuses:
				pctx, cancel := context.WithTimeout(ctx, callTimeout)
				s.postStatus(pctx, m)
				cancel()
			}
		}
	}()
}

func (s *Service) postAnswer(ctx context.Context, m *store.TAKChatMessage) {
	op, err := s.store.GetMatrixOperator(ctx, m.TenantID, m.UserID)
	if err != nil || op == nil {
		return // a console operator's answer, or one older than the row
	}
	if ok, err := s.store.ClaimOnce(ctx, "matrix:answer:"+m.TenantID+":"+m.ID); err != nil || !ok {
		return
	}
	drop := func(why string, args ...any) {
		answersTotal.WithLabelValues("dropped").Inc()
		s.log.Warn("matrix: a crew's answer was not posted: "+why, append([]any{"tenant", m.TenantID}, args...)...)
	}
	c := s.roomClient(ctx, m.TenantID, op.RoomID, drop)
	if c == nil {
		return
	}
	if err := c.send(ctx, op.RoomID, "m.room.message", "answer-"+m.ID, map[string]any{
		"msgtype":    "m.text",
		"body":       s.callsign(ctx, m) + ": " + m.Text,
		"m.mentions": map[string]any{"user_ids": []string{op.MatrixUserID}},
	}); err != nil {
		answersTotal.WithLabelValues("failed").Inc()
		s.log.Warn("matrix: a crew's answer could not be posted", "tenant", m.TenantID, "room", op.RoomID, "error", err)
		return
	}
	answersTotal.WithLabelValues("posted").Inc()
}

// postStatus puts a kit's word beside the message in the room it was
// written in (MESHSAT-1746): ❌ on the message and one notice for a message
// the kit did not deliver, ✅✅ for one the far end confirmed. Claimed once
// per message and status, because every replica records the status and
// calls the hook.
func (s *Service) postStatus(ctx context.Context, m *store.TAKChatMessage) {
	op, err := s.store.GetMatrixOperator(ctx, m.TenantID, m.UserID)
	if err != nil || op == nil {
		return
	}
	eventID, ok := strings.CutPrefix(m.ClientRef, "matrix:")
	if !ok || eventID == "" {
		return // written before the event id was kept with the message
	}
	if ok, err := s.store.ClaimOnce(ctx, "matrix:status:"+m.TenantID+":"+m.ID+":"+m.Status); err != nil || !ok {
		return
	}
	drop := func(why string, args ...any) {
		statusesTotal.WithLabelValues("dropped").Inc()
		s.log.Warn("matrix: a kit's word on a message was not posted: "+why, append([]any{"tenant", m.TenantID}, args...)...)
	}
	c := s.roomClient(ctx, m.TenantID, op.RoomID, drop)
	if c == nil {
		return
	}
	react := func(key, txn string) error {
		return c.send(ctx, op.RoomID, "m.reaction", txn, map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": eventID, "key": key},
		})
	}
	if m.Status == takchat.StatusDelivered {
		err = react("✅✅", "ack2-"+eventID)
	} else if err = react("❌", "nack-"+eventID); err == nil {
		why := strings.TrimRight(strings.TrimSpace(m.StatusDetail), ".")
		if why == "" {
			why = "the kit did not deliver it"
		}
		if ok, err := s.store.TakePersonChatSlot(ctx, m.TenantID,
			"mx:notice:"+roomKey(op.RoomID)+":"+s.now().UTC().Format("200601021504"), noticesPerMinute); err != nil || !ok {
			drop("the room has had its notices for this minute", "room", op.RoomID)
			return
		}
		err = c.send(ctx, op.RoomID, "m.room.message", "status-"+m.ID+"-"+m.Status, map[string]any{
			"msgtype":    "m.notice",
			"body":       "Not delivered to " + s.callsign(ctx, m) + ": " + why + ".",
			"m.mentions": map[string]any{"user_ids": []string{op.MatrixUserID}},
		})
	}
	if err != nil {
		statusesTotal.WithLabelValues("failed").Inc()
		s.log.Warn("matrix: a kit's word on a message could not be posted", "tenant", m.TenantID, "room", op.RoomID, "error", err)
		return
	}
	statusesTotal.WithLabelValues("posted").Inc()
}

// roomClient answers a client that may post into one of the tenant's listed,
// unencrypted rooms, or nil after telling drop why not.
func (s *Service) roomClient(ctx context.Context, tenantID, room string, drop func(why string, args ...any)) *client {
	acct, err := s.accounts.ForTenant(ctx, tenantID, integrations.ProviderMatrix)
	if err != nil || acct == nil || acct.Platform {
		drop("the tenant has no Matrix account any more")
		return nil
	}
	cfg, err := parseConfig(acct)
	if err != nil {
		drop("the Matrix account cannot be used", "error", err)
		return nil
	}
	if !cfg.roomSet[room] {
		drop("the room is no longer listed", "room", room)
		return nil
	}
	c := newClient(cfg.homeserver, cfg.token)
	if enc, err := c.encrypted(ctx, room); err != nil || enc {
		drop("the room is encrypted, or its state cannot be read", "room", room, "error", err)
		return nil
	}
	return c
}

// callsign is the crew's callsign as the room knows it, else its uid.
func (s *Service) callsign(ctx context.Context, m *store.TAKChatMessage) string {
	if crew, err := s.store.GetTAKContact(ctx, m.TenantID, m.CrewUID); err == nil && crew != nil && crew.Callsign != "" {
		return crew.Callsign
	}
	return m.CrewUID
}

// --- checking an account, for the Settings page --------------------------------

// Check signs in with a tenant's Matrix account, joins its rooms and reports
// what it found, for the Test button.
func Check(ctx context.Context, a *integrations.Account) (bool, string) {
	cfg, err := parseConfig(a)
	if err != nil {
		return false, err.Error()
	}
	c := newClient(cfg.homeserver, cfg.token)
	bot, err := c.whoami(ctx)
	if err != nil {
		return false, "The homeserver did not accept the access token: " + err.Error()
	}
	ok := true
	parts := make([]string, 0, len(cfg.rooms))
	for _, room := range cfg.rooms {
		if err := c.join(ctx, room); err != nil {
			ok = false
			parts = append(parts, room+": the bot cannot join it (invite it first): "+err.Error())
			continue
		}
		enc, err := c.encrypted(ctx, room)
		switch {
		case err != nil:
			ok = false
			parts = append(parts, room+": its settings cannot be read: "+err.Error())
		case enc:
			ok = false
			parts = append(parts, room+": encrypted, which the bridge does not support yet")
		default:
			parts = append(parts, room+": ready")
		}
	}
	return ok, "Signed in as " + bot + ". " + strings.Join(parts, "; ") + "."
}

// --- text --------------------------------------------------------------------

// matchCrew finds the crew a message is addressed to: a crew's callsign in any
// case, then a space, colon or comma, or the end. The longest callsign wins, so
// "@Team 2 go" reaches "Team 2" rather than "Team"; of two crews with one
// callsign, the one seen last (crews come newest first).
func matchCrew(crews []store.TAKContact, s string) (*store.TAKContact, string) {
	var best *store.TAKContact
	for i := range crews {
		cs := crews[i].Callsign
		if cs == "" || len(cs) > len(s) || !strings.EqualFold(s[:len(cs)], cs) {
			continue
		}
		if len(s) > len(cs) && !strings.ContainsRune(" :,\t\n", rune(s[len(cs)])) {
			continue
		}
		if best == nil || len(cs) > len(best.Callsign) {
			best = &crews[i]
		}
	}
	if best == nil {
		return nil, ""
	}
	return best, strings.TrimSpace(strings.TrimLeft(s[len(best.Callsign):], " :,\t\n"))
}

func firstToken(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return strings.TrimRight(f[0], ":,")
	}
	return ""
}

// stripReplyFallback drops the quoted lines a client puts in front of a reply
// ("> <@anna:example.org> earlier text", then an empty line).
func stripReplyFallback(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], ">") {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

func crewList(crews []store.TAKContact) string {
	seen := map[string]bool{}
	var names []string
	for _, c := range crews {
		if c.Callsign != "" && !seen[c.Callsign] {
			seen[c.Callsign] = true
			names = append(names, c.Callsign)
		}
	}
	if len(names) == 0 {
		return "No crew has been seen in the last day."
	}
	sort.Strings(names)
	if len(names) > 30 {
		names = append(names[:30], "…")
	}
	return "Crews seen in the last day: " + strings.Join(names, ", ") + ". Write @ and a callsign, then your message."
}

// operatorID is the operator id a Matrix user writes to crews under in one
// room of one tenant: answers go back to the room they were asked from.
func operatorID(tenantID, room, user string) string {
	sum := sha256.Sum256([]byte(tenantID + "|" + room + "|" + user))
	return OperatorPrefix + hex.EncodeToString(sum[:8])
}

func roomKey(room string) string {
	sum := sha256.Sum256([]byte(room))
	return hex.EncodeToString(sum[:8])
}
