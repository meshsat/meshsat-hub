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

func init() {
	for s := range kitStatuses {
		statusTotal.WithLabelValues(s)
	}
	statusTotal.WithLabelValues("refused")
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
	if b, err := s.store.GetBridge(ctx, req.TenantID, crew.BridgeID); err != nil || b == nil {
		return nil, false, ErrUnregistered
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

	op := OperatorPeer(req.UserID, req.Name)
	id := messageID(req.TenantID, req.UserID, req.ClientRef)
	ev := tak.BuildGeoChat(op, tak.ChatPeer{UID: crew.UID, Callsign: crew.Callsign}, text, id, now)
	xml, err := tak.MarshalCotEvent(ev)
	if err != nil {
		return nil, false, err
	}
	msg := &store.TAKChatMessage{
		TenantID: req.TenantID, ID: id, CrewUID: crew.UID, OperatorUID: op.UID, UserID: req.UserID,
		Direction: "out", Text: text, BridgeID: crew.BridgeID, Status: "sent", ClientRef: req.ClientRef, CreatedAt: now,
	}
	created, err := s.store.SaveTAKChatMessage(ctx, msg)
	if err != nil {
		return nil, false, err
	}
	if !created {
		return msg, false, nil
	}
	if err := s.bus.Publish(hubmqtt.TopicTAKCotInFor(req.TenantID, crew.BridgeID), 1, false, xml); err != nil {
		const detail = "the message could not be handed to the kit"
		_ = s.store.SetTAKChatMessageStatus(ctx, req.TenantID, id, "failed", detail)
		msg.Status, msg.StatusDetail = "failed", detail
		s.log.Warn("takchat: a message to a crew could not be published",
			"tenant", req.TenantID, "bridge", crew.BridgeID, "error", err)
		return msg, true, ErrPublish
	}
	return msg, true, nil
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
		msg := &store.TAKChatMessage{
			TenantID: tenantID, ID: id, CrewUID: crewUID, OperatorUID: d.UID, UserID: userID,
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
