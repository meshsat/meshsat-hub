// Package takchat lets an operator of a tenant write to one crew from the Hub's
// map, and keeps the crew's answers (MESHSAT-1706).
//
// A crew is anything a client of the tenant puts on the TAK map: a Meshtastic
// node behind a kit, or a MeshSat phone. The leader's TAK forwarder knows which
// client owns each crew and writes it to the store (tak_contacts). A message an
// operator writes is a GeoChat from the operator's own TAK identity
// ("meshsat-hub-op-<user id>", callsign the operator's name) to the crew, handed
// to the owning client alone on its {ns}/bridge/{id}/tak/cot/in: the kit sends it
// to the node as a direct message, the phone shows it. The crew's answer comes
// back on the client's tak/cot/out as a GeoChat addressed to the operator, and
// ReplySink stores it.
//
// # Every replica
//
// Send runs on whichever replica the request reached; it needs only the store
// and the bus. ReplySink runs on both replicas, because both receive every
// export: the answer is stored under the GeoChat's own message id, so both
// writing it is one row (the observation case, .claude/rules/replicas.md).
package takchat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

const (
	// MaxBytes is the most a message may be: it goes to a radio, which carries
	// about 230 bytes, with the operator's name in front.
	MaxBytes = 200
	// crewFresh is how long after its last export a crew can be written to.
	crewFresh = 24 * time.Hour
	// Per crew one message per crewWindow; per tenant tenantPerHour an hour.
	// Each message is a transmission on a duty-cycled radio.
	crewWindow    = 10 * time.Second
	tenantPerHour = 60
	// maxCallsign bounds the operator's name as the crew sees it.
	maxCallsign = 24
	// replyQueue bounds what the reply sink may have waiting.
	replyQueue = 256
	// statusMaxBytes bounds a kit's status report on the wire; one is under
	// 400 bytes. statusDetailMax bounds the line kept for the dispatcher.
	statusMaxBytes  = 4096
	statusDetailMax = 200
)

// The kit's words on a message (MESHSAT-1746, hubmqtt.TAKChatStatus).
const (
	StatusQueued      = "queued"
	StatusDelivered   = "delivered"
	StatusFailed      = "failed"
	StatusUndelivered = "undelivered"
)

var kitStatuses = map[string]bool{StatusQueued: true, StatusDelivered: true, StatusFailed: true, StatusUndelivered: true}

var statusTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_takchat_status_total",
	Help: "Kits' words on what became of a dispatcher's chat, by status; refused is one the Hub did not record (MESHSAT-1746).",
}, []string{"status"})

// sentTotal counts messages by the bearer they left on, and frames that came
// back by bearer and kind (MESHSAT-1744).
var sentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_takchat_sent_total",
	Help: "Dispatchers' chats handed to kits, by bearer and result (MESHSAT-1744).",
}, []string{"bearer", "result"})

var frameTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_takchat_frame_total",
	Help: "Crew chat frames a kit sent over satellite or SMS, by bearer, kind (answer, status) and result (MESHSAT-1744).",
}, []string{"bearer", "kind", "result"})

func init() {
	for s := range kitStatuses {
		statusTotal.WithLabelValues(s)
	}
	statusTotal.WithLabelValues("refused")
	for _, b := range []string{BearerMQTT, BearerSBD, BearerIMT, BearerSMS} {
		sentTotal.WithLabelValues(b, "sent")
		sentTotal.WithLabelValues(b, "failed")
	}
}

// The ways a send is refused; the API maps each to a status.
var (
	ErrDisabled     = errors.New("takchat: dispatcher-to-crew chat is not switched on")
	ErrEmpty        = errors.New("takchat: no text")
	ErrTooLong      = fmt.Errorf("takchat: more than %d bytes", MaxBytes)
	ErrNoCrew       = errors.New("takchat: no such crew")
	ErrStale        = errors.New("takchat: the crew has not been seen for a day")
	ErrUnregistered = errors.New("takchat: the client that owns the crew is no longer registered")
	ErrLimited      = errors.New("takchat: too many messages; wait")
	ErrPublish      = errors.New("takchat: the message could not be handed to the bus")
)

// Bus is what the service needs of the message bus.
type Bus interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
	Subscribe(topic string, qos byte, handler bus.MessageHandler) error
}

// Service sends operators' messages and keeps crews' answers.
type Service struct {
	store    store.Store
	bus      Bus
	log      *slog.Logger
	enabled  bool
	now      func() time.Time
	bearers  Bearers
	replies  chan reply
	statuses chan reply
	onReply  func(ctx context.Context, m *store.TAKChatMessage)
	onStatus func(ctx context.Context, m *store.TAKChatMessage)
}

// New returns a Service. enabled is HUB_TAKCHAT_ENABLED.
func New(s store.Store, b Bus, enabled bool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: s, bus: b, log: log, enabled: enabled, now: time.Now,
		replies: make(chan reply, replyQueue), statuses: make(chan reply, replyQueue)}
}

// Enabled reports whether the feature is switched on.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// SetReplyHook makes fn run for each crew's answer this replica stored first
// (MESHSAT-1736: the Matrix bridge posts it into the room the question came
// from). Only the write that created the row calls it, so of two replicas
// storing one answer, one does. fn runs on the sink's goroutine and must hand
// slow work off. Call it before StartReplySink.
func (s *Service) SetReplyHook(fn func(ctx context.Context, m *store.TAKChatMessage)) {
	s.onReply = fn
}

// SetStatusHook makes fn run with the message as it is after a kit's word on
// it was recorded (MESHSAT-1746). Unlike the reply hook it runs on EVERY
// replica that recorded it: the write is an observation both make, so fn
// claims its own side effect (the Matrix bridge does, per message and
// status). fn runs on the sink's goroutine and must hand slow work off.
// Call it before StartReplySink.
func (s *Service) SetStatusHook(fn func(ctx context.Context, m *store.TAKChatMessage)) {
	s.onStatus = fn
}

// SendRequest is one message an operator writes to a crew.
type SendRequest struct {
	TenantID string
	CrewUID  string
	UserID   string
	// Name is the operator's name as the crew sees it; an email is never used.
	Name string
	Text string
	// ClientRef makes the request idempotent: the same operator sending the
	// same ref again stores and sends nothing more.
	ClientRef string
}

// Send writes one message to a crew. It answers the stored message and whether
// this request created it (false for a repeat of the same client ref).
func (s *Service) Send(ctx context.Context, req SendRequest) (*store.TAKChatMessage, bool, error) {
	if !s.Enabled() {
		return nil, false, ErrDisabled
	}
	text := foldControls(req.Text)
	switch {
	case text == "":
		return nil, false, ErrEmpty
	case len(text) > MaxBytes:
		return nil, false, ErrTooLong
	}
	now := s.now()
	crew, err := s.store.GetTAKContact(ctx, req.TenantID, req.CrewUID)
	if err != nil {
		return nil, false, err
	}
	if crew == nil {
		return nil, false, ErrNoCrew
	}
	if now.Sub(crew.LastSeen) > crewFresh {
		return nil, false, ErrStale
	}
	b, err := s.store.GetBridge(ctx, req.TenantID, crew.BridgeID)
	if err != nil || b == nil {
		return nil, false, ErrUnregistered
	}
	// How it goes (MESHSAT-1744): MQTT while the kit is on it, else a frame
	// on a bearer the workspace allows. Decided, and the frame built and
	// measured, before anything is stored, so a refusal stores nothing.
	tenant, err := s.store.GetTenant(ctx, req.TenantID)
	if err != nil {
		return nil, false, err
	}
	op := OperatorPeer(req.UserID, req.Name)
	id := messageID(req.TenantID, req.UserID, req.ClientRef)
	pick, err := s.chooseBearer(ctx, tenant, b, now)
	if err != nil {
		return nil, false, err
	}
	var frame []byte
	if pick.Bearer != BearerMQTT {
		node, toKit, ok := CrewNode(crew.UID)
		if !ok {
			return nil, false, fmt.Errorf("%w (%s cannot be named in a frame)", ErrNoBearer, crew.UID)
		}
		frame, err = EncodeChat(Chat{MessageID: id, Node: node, ToKit: toKit, ReplyHere: pick.Forced,
			OperatorUID: op.UID, Callsign: op.Callsign, Text: text})
		if err != nil {
			return nil, false, err
		}
		if n := MaxFrame(pick.Bearer); len(frame) > n {
			return nil, false, fmt.Errorf("%w: %d bytes as a frame, %s carries %d", ErrFrameTooLong, len(frame), pick.Bearer, n)
		}
	}
	// The ceilings hold across both replicas: one statement per bucket.
	if ok, err := s.store.TakePersonChatSlot(ctx, req.TenantID,
		"tak:crew:"+crew.UID+":"+strconv.FormatInt(now.Unix()/int64(crewWindow/time.Second), 10), 1); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, ErrLimited
	}
	if ok, err := s.store.TakePersonChatSlot(ctx, req.TenantID,
		"tak:tenant:"+now.UTC().Format("2006010215"), tenantPerHour); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, ErrLimited
	}

	ev := tak.BuildGeoChat(op, tak.ChatPeer{UID: crew.UID, Callsign: crew.Callsign}, text, id, now)
	xml, err := tak.MarshalCotEvent(ev)
	if err != nil {
		return nil, false, err
	}
	msg := &store.TAKChatMessage{
		TenantID: req.TenantID, ID: id, CrewUID: crew.UID, OperatorUID: op.UID, UserID: req.UserID,
		Direction: "out", Text: text, BridgeID: crew.BridgeID, Status: "sent", ClientRef: req.ClientRef,
		Bearer: pick.Bearer, CreatedAt: now,
	}
	created, err := s.store.SaveTAKChatMessage(ctx, msg)
	if err != nil {
		return nil, false, err
	}
	if !created {
		return msg, false, nil
	}
	fail := func(detail string, err error) (*store.TAKChatMessage, bool, error) {
		_ = s.store.SetTAKChatMessageStatus(ctx, req.TenantID, id, "failed", detail)
		msg.Status, msg.StatusDetail = "failed", detail
		sentTotal.WithLabelValues(pick.Bearer, "failed").Inc()
		s.log.Warn("takchat: a message to a crew could not be sent",
			"tenant", req.TenantID, "bridge", crew.BridgeID, "bearer", pick.Bearer, "error", err)
		return msg, true, ErrPublish
	}
	if pick.Bearer == BearerMQTT {
		if err := s.bus.Publish(hubmqtt.TopicTAKCotInFor(req.TenantID, crew.BridgeID), 1, false, xml); err != nil {
			return fail("the message could not be handed to the kit", err)
		}
		sentTotal.WithLabelValues(BearerMQTT, "sent").Inc()
		return msg, true, nil
	}
	// A frame bearer costs money: claimed once per message and bearer, so a
	// retry of this request on any replica sends nothing more.
	if ok, err := s.store.ClaimOnce(ctx, "takchat:out:"+req.TenantID+":"+id+":"+pick.Bearer); err != nil {
		return fail("the send could not be claimed", err)
	} else if !ok {
		return msg, true, nil
	}
	switch pick.Bearer {
	case BearerSMS:
		err = s.bearers.SendSMS(ctx, req.TenantID, crew.BridgeID, base64.StdEncoding.EncodeToString(frame))
	default:
		err = s.bearers.SendMT(ctx, req.TenantID, pick.IMEI, frame)
	}
	if err != nil {
		return fail("the message could not be sent over "+bearerName(pick.Bearer)+": "+clipDetail(err.Error()), err)
	}
	sentTotal.WithLabelValues(pick.Bearer, "sent").Inc()
	s.log.Info("takchat: a message to a crew went as a frame", "tenant", req.TenantID, "bridge", crew.BridgeID,
		"bearer", pick.Bearer, "bytes", len(frame), "forced", pick.Forced)
	return msg, true, nil
}

// bearerName is a bearer as a person reads it.
func bearerName(bearer string) string {
	switch bearer {
	case BearerSBD, BearerIMT:
		return "satellite"
	case BearerSMS:
		return "SMS"
	}
	return bearer
}

// IsSatellite reports whether a bearer is a satellite modem's.
func IsSatellite(bearer string) bool { return bearer == BearerSBD || bearer == BearerIMT }

func clipDetail(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= statusDetailMax {
		return s
	}
	return string([]rune(s)[:statusDetailMax])
}

// OperatorPeer is the TAK identity an operator writes under: a uid the Hub
// alone may use, and a callsign that is the operator's name, never an email,
// at most maxCallsign characters.
func OperatorPeer(userID, name string) tak.ChatPeer {
	var b strings.Builder
	for _, r := range userID {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			b.WriteRune(r)
		}
	}
	id := b.String()
	if id == "" {
		id = "api"
	}
	callsign := strings.TrimSpace(foldControls(name))
	if callsign == "" || strings.Contains(callsign, "@") {
		callsign = "Office"
	}
	if utf8.RuneCountInString(callsign) > maxCallsign {
		callsign = string([]rune(callsign)[:maxCallsign])
	}
	return tak.ChatPeer{UID: tak.OperatorUIDPrefix + id, Callsign: callsign}
}

// messageID is a message's GeoChat id: derived from the client ref when there is
// one, so a resent request is the same message, else random.
func messageID(tenantID, userID, clientRef string) string {
	var b []byte
	if clientRef != "" {
		sum := sha256.Sum256([]byte(tenantID + "|" + userID + "|" + clientRef))
		b = sum[:16]
	} else {
		b = make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return strconv.FormatInt(time.Now().UnixNano(), 36)
		}
	}
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// foldControls turns line breaks and tabs into spaces and drops other control
// characters: a radio shows one line.
func foldControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || r == utf8.RuneError:
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// --- the answers -------------------------------------------------------------

type reply struct {
	topic   string
	payload []byte
}

// StartReplySink subscribes to every client's export and stores the answers
// addressed to an operator, and to every kit's word on a chat it was handed
// (MESHSAT-1746) and records it on the message, until ctx is done. It runs
// on every replica.
func (s *Service) StartReplySink(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	for _, filter := range hubmqtt.TAKCotOutFilters() {
		if err := s.bus.Subscribe(filter, 0, s.receive); err != nil {
			return fmt.Errorf("takchat: subscribe %s: %w", filter, err)
		}
	}
	for _, filter := range hubmqtt.TAKChatStatusFilters() {
		if err := s.bus.Subscribe(filter, 1, s.receiveStatus); err != nil {
			return fmt.Errorf("takchat: subscribe %s: %w", filter, err)
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-s.replies:
				s.store1(ctx, r)
			case r := <-s.statuses:
				s.record1(ctx, r)
			}
		}
	}()
	return nil
}

// receive runs on the bus goroutine: a byte test and a handover, nothing else.
func (s *Service) receive(topic string, payload []byte) {
	if !bytes.Contains(payload, []byte(tak.OperatorUIDPrefix)) {
		return
	}
	select {
	case s.replies <- reply{topic: topic, payload: append([]byte(nil), payload...)}:
	default:
		s.log.Warn("takchat: the reply sink is behind, an answer was not stored")
	}
}

// receiveStatus runs on the bus goroutine: a size test and a handover.
func (s *Service) receiveStatus(topic string, payload []byte) {
	if len(payload) == 0 || len(payload) > statusMaxBytes {
		return
	}
	select {
	case s.statuses <- reply{topic: topic, payload: append([]byte(nil), payload...)}:
	default:
		s.log.Warn("takchat: the status sink is behind, a kit's word was not recorded")
	}
}

// record1 records one kit's word on a message. The kit must be registered
// to the topic's tenant and be the one the message was handed to, which the
// store checks against the row (SetTAKChatMessageOutcome); the status must
// be one of the four words. Both replicas record it: the same fact twice is
// one row, and the hook claims its own side effect.
func (s *Service) record1(ctx context.Context, r reply) {
	tenantID, bridgeID, ok := hubmqtt.ParseTAKChatStatus(r.topic)
	if !ok {
		return
	}
	refuse := func(why string, args ...any) {
		statusTotal.WithLabelValues("refused").Inc()
		s.log.Debug("takchat: a kit's word on a message was not recorded: "+why,
			append([]any{"tenant", tenantID, "bridge", bridgeID}, args...)...)
	}
	var st hubmqtt.TAKChatStatus
	if err := json.Unmarshal(r.payload, &st); err != nil {
		refuse("not JSON", "error", err)
		return
	}
	st.MessageID = strings.TrimSpace(st.MessageID)
	if st.MessageID == "" || len(st.MessageID) > 96 || !kitStatuses[st.Status] {
		refuse("no message id, or not one of the four words", "status", st.Status)
		return
	}
	if b, err := s.store.GetBridge(ctx, tenantID, bridgeID); err != nil || b == nil {
		refuse("the bridge is not registered to the tenant")
		return
	}
	detail := foldControls(st.Detail)
	if utf8.RuneCountInString(detail) > statusDetailMax {
		detail = string([]rune(detail)[:statusDetailMax])
	}
	if detail == "" && st.Reason != "" {
		detail = foldControls(st.Reason)
	}
	msg, err := s.store.SetTAKChatMessageOutcome(ctx, tenantID, bridgeID, st.MessageID, st.Status, detail)
	if err != nil {
		s.log.Warn("takchat: could not record a kit's word on a message", "tenant", tenantID, "error", err)
		return
	}
	if msg == nil {
		refuse("no message of this id was handed to the bridge, or a queued came after a final word", "message", st.MessageID)
		return
	}
	statusTotal.WithLabelValues(st.Status).Inc()
	if s.onStatus != nil {
		s.onStatus(ctx, msg)
	}
}

// store1 stores one export that may be an answer to an operator. It must come
// from a registered client of the topic's tenant, be a chat, name an operator,
// and come from a crew that client owns: a client cannot answer in another
// client's crew's name.
func (s *Service) store1(ctx context.Context, r reply) {
	tenantID, bridgeID, ok := hubmqtt.ParseTAKCotOut(r.topic)
	if !ok {
		return
	}
	now := s.now()
	_, meta, err := tak.Sanitize(r.payload, tak.ClientLimits, now)
	if err != nil || !meta.IsChat() {
		return
	}
	crewUID, _ := meta.Sender()
	text := foldControls(meta.Remarks)
	if crewUID == "" || text == "" {
		return
	}
	b, err := s.store.GetBridge(ctx, tenantID, bridgeID)
	if err != nil || b == nil {
		return
	}
	crew, err := s.store.GetTAKContact(ctx, tenantID, crewUID)
	if err != nil || crew == nil || crew.BridgeID != bridgeID {
		return
	}
	id := meta.ChatMessageID
	if id == "" {
		id = meta.UID
	}
	for _, d := range meta.Addressees() {
		userID, ok := strings.CutPrefix(d.UID, tak.OperatorUIDPrefix)
		if !ok {
			continue
		}
		operatorUID := d.UID
		// A kit that was written to over satellite or SMS knows the
		// operator by hash alone (MESHSAT-1744): the operator it stands
		// for is the one who last wrote to this crew under that hash.
		if h, hashed := HashFromOperatorUID(d.UID); hashed {
			real, found := s.operatorFromHash(ctx, tenantID, crewUID, h)
			if !found {
				s.log.Info("takchat: an answer names an operator hash nobody wrote to this crew under", "tenant", tenantID, "crew", crewUID)
				return
			}
			operatorUID, userID = real, strings.TrimPrefix(real, tak.OperatorUIDPrefix)
		}
		msg := &store.TAKChatMessage{
			TenantID: tenantID, ID: id, CrewUID: crewUID, OperatorUID: operatorUID, UserID: userID,
			Direction: "in", Text: text, BridgeID: bridgeID, Status: "received", CreatedAt: now,
		}
		created, err := s.store.SaveTAKChatMessage(ctx, msg)
		if err != nil {
			s.log.Warn("takchat: could not store a crew's answer", "tenant", tenantID, "error", err)
			return
		}
		if created && s.onReply != nil {
			s.onReply(ctx, msg)
		}
		return
	}
}

// --- the frames a kit sends back (MESHSAT-1744) ------------------------------

// operatorFromHash finds the operator who last wrote to crewUID under hash h:
// the newest "out" message to that crew whose operator uid hashes to it.
func (s *Service) operatorFromHash(ctx context.Context, tenantID, crewUID string, h [8]byte) (string, bool) {
	msgs, err := s.store.ListTAKChatMessages(ctx, tenantID, crewUID, 100)
	if err != nil {
		return "", false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Direction == "out" && OperatorHash(msgs[i].OperatorUID) == h {
			return msgs[i].OperatorUID, true
		}
	}
	return "", false
}

// question finds the "out" message a kit's frame names and checks that the
// frame came from the kit it was handed to: over a satellite modem, the
// modem must belong to that bridge; over SMS, the sender must be the kit's
// number when one is on file (the message id, 128 random bits, is the
// capability otherwise, as the "@" lane's ref is). why is non-empty when the
// frame is refused.
func (s *Service) question(ctx context.Context, tenantID, bearer, origin, messageID string, node uint32) (*store.TAKChatMessage, string) {
	msg, err := s.store.GetTAKChatMessage(ctx, tenantID, messageID)
	if err != nil {
		return nil, "lookup failed: " + err.Error()
	}
	if msg == nil || msg.Direction != "out" || msg.BridgeID == "" {
		return nil, "no message of this id was handed to a kit of this tenant"
	}
	if crewNode, _, ok := CrewNode(msg.CrewUID); !ok || crewNode != node {
		return nil, "the frame's node is not the message's crew"
	}
	switch bearer {
	case BearerSMS:
		peer, err := s.store.GetOOBPeer(ctx, tenantID, msg.BridgeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, "the kit's pairing could not be read"
		}
		if peer != nil && peer.Phone != "" && peer.Phone != origin {
			return nil, "the SMS came from " + origin + ", the kit's number is on file as another"
		}
	default:
		owner, err := s.store.DeviceBridgeID(ctx, tenantID, origin)
		if err != nil || owner != msg.BridgeID {
			return nil, "modem " + origin + " does not belong to the kit the message was handed to"
		}
	}
	return msg, ""
}

// CrewAnswer takes a C2 frame's payload (bridge.UplinkSink): the crew's
// answer to one question, stored as the answer a kit sends over MQTT is,
// under an id both replicas derive the same way, and handed to the reply
// hook. It answers the bridge the frame is taken as, "" when refused.
func (s *Service) CrewAnswer(ctx context.Context, tenantID, bearer, origin string, payload []byte) string {
	if !s.Enabled() {
		return ""
	}
	refuse := func(why string, args ...any) string {
		frameTotal.WithLabelValues(bearer, "answer", "refused").Inc()
		s.log.Warn("takchat: a crew's answer frame was not taken: "+why,
			append([]any{"tenant", tenantID, "bearer", bearer, "origin", origin}, args...)...)
		return ""
	}
	a, err := DecodeAnswer(payload)
	if err != nil {
		return refuse("unreadable", "error", err)
	}
	text := foldControls(a.Text)
	if text == "" {
		return refuse("no words")
	}
	q, why := s.question(ctx, tenantID, bearer, origin, a.MessageID, a.Node)
	if why != "" {
		return refuse(why, "message", a.MessageID)
	}
	sum := sha256.Sum256([]byte("takchat:answer:" + tenantID + "|" + q.ID + "|" + CrewUIDOf(a.Node, false) + "|" + strconv.FormatInt(a.At.Unix(), 10)))
	var idb [16]byte
	copy(idb[:], sum[:16])
	id := hubmsgUUID(idb)
	msg := &store.TAKChatMessage{
		TenantID: tenantID, ID: id, CrewUID: q.CrewUID, OperatorUID: q.OperatorUID, UserID: q.UserID,
		Direction: "in", Text: text, BridgeID: q.BridgeID, Status: "received", Bearer: bearer, CreatedAt: s.now(),
	}
	created, err := s.store.SaveTAKChatMessage(ctx, msg)
	if err != nil {
		return refuse("not stored", "error", err)
	}
	frameTotal.WithLabelValues(bearer, "answer", "stored").Inc()
	s.log.Info("takchat: a crew's answer came as a frame", "tenant", tenantID, "bridge", q.BridgeID, "bearer", bearer, "crew", q.CrewUID)
	if created && s.onReply != nil {
		s.onReply(ctx, msg)
	}
	return q.BridgeID
}

// CrewStatus takes a C3 frame's payload (bridge.UplinkSink): the kit's word
// on one chat, recorded as the word it sends on tak/chat/status is. It
// answers the bridge the frame is taken as, "" when refused.
func (s *Service) CrewStatus(ctx context.Context, tenantID, bearer, origin string, payload []byte) string {
	if !s.Enabled() {
		return ""
	}
	refuse := func(why string, args ...any) string {
		frameTotal.WithLabelValues(bearer, "status", "refused").Inc()
		statusTotal.WithLabelValues("refused").Inc()
		s.log.Warn("takchat: a kit's status frame was not taken: "+why,
			append([]any{"tenant", tenantID, "bearer", bearer, "origin", origin}, args...)...)
		return ""
	}
	st, err := DecodeStatus(payload)
	if err != nil {
		return refuse("unreadable", "error", err)
	}
	q, why := s.question(ctx, tenantID, bearer, origin, st.MessageID, st.Node)
	if why != "" {
		return refuse(why, "message", st.MessageID)
	}
	detail := foldControls(st.Detail)
	if detail == "" && st.Reason != "" {
		detail = st.Reason
	}
	msg, err := s.store.SetTAKChatMessageOutcome(ctx, tenantID, q.BridgeID, q.ID, st.Status, detail)
	if err != nil {
		return refuse("not recorded", "error", err)
	}
	if msg == nil {
		return refuse("a queued came after a final word", "message", st.MessageID)
	}
	frameTotal.WithLabelValues(bearer, "status", "recorded").Inc()
	statusTotal.WithLabelValues(st.Status).Inc()
	s.log.Info("takchat: a kit's word on a chat came as a frame", "tenant", tenantID, "bridge", q.BridgeID,
		"bearer", bearer, "status", st.Status, "reason", st.Reason)
	if s.onStatus != nil {
		s.onStatus(ctx, msg)
	}
	return q.BridgeID
}
