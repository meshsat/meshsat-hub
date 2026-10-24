package personchat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/email"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/satchat"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Who may open a conversation: a satellite modem REGISTERED to the tenant,
// and only a message that really came through the Hub's own satellite
// provider handlers. Every bridge of a tenant may publish mo/decoded for any
// device id in its namespace, so the lane SENDS only on the provider's copy
// (hubmqtt.TopicSatMOFor), which no bridge can publish; the routing engine's
// claim reads mo/decoded with the same rules, so a route never also sends
// what the lane sends. Never an SMS from a phone number (anybody can text the
// Hub), never an unregistered modem (its tenant is only a guess).
//
// A kit's own SIM is not an originator in this version. A kit texts a person
// with its own modem directly, and the Bridge drops an SMS shaped
// "[+31...] text" as an echo of its own message.
//
// Who may be written to is the tenant's choice (store.Tenant.PersonChatMode):
// "contacts" (the default) only the addresses in the tenant's directory, the
// people it vouched for, because a satellite text is whatever somebody typed
// on a mesh; "open" any number or address; "off" nobody.

const (
	ModeOff      = "off"
	ModeContacts = "contacts"
	ModeOpen     = "open"
)

// Store is the slice of store.Store the lane needs.
type Store interface {
	ClaimOnce(ctx context.Context, key string) (bool, error)
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
	GetDevice(ctx context.Context, tenantID, imei string) (*store.Device, error)
	ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error)
	GetOOBPeer(ctx context.Context, tenantID, bridgeID string) (*store.OOBPeer, error)
	SavePersonConversationOut(ctx context.Context, c *store.PersonConversation) (*store.PersonConversation, error)
	GetPersonConversationByRef(ctx context.Context, tenantID, ref string) (*store.PersonConversation, error)
	PersonConversationsWith(ctx context.Context, tenantID, peerKind, peer string, since time.Time) ([]store.PersonConversation, error)
	MarkPersonConversationIn(ctx context.Context, tenantID, id string, at time.Time, messageID string) error
	MarkPersonConversationOut(ctx context.Context, tenantID, id string, at time.Time, messageID string) error
	RecordPersonChatSend(ctx context.Context, s *store.PersonChatSend) error
	TakePersonChatSlot(ctx context.Context, tenantID, key string, max int) (bool, error)
}

// Directory is the tenant's contacts.
type Directory interface {
	ListContacts(ctx context.Context, tenantID string) ([]directory.Contact, error)
}

// TenantResolver answers which tenant owns a modem.
type TenantResolver interface {
	ForDeviceTopic(ctx context.Context, imei, topicTenant string) string
}

// SMS sends one text from the tenant's own number.
type SMS interface {
	Send(ctx context.Context, to, body string) error
	From() string
}

// Mail sends one mail from the tenant's own gateway.
type Mail interface {
	SendMessage(ctx context.Context, m email.Message) (string, error)
	From() string
}

// Accounts finds a tenant's own sending accounts; nil when it has none. Never
// the platform's for a customer tenant (the pools' rule).
type Accounts interface {
	SMS(ctx context.Context, tenantID string) SMS
	Mail(ctx context.Context, tenantID string) (m Mail, replyTo string)
}

// SatSender sends text to one modem as an MT.
type SatSender func(ctx context.Context, tenantID, imei, text string) error

// Options are the platform's settings (config.PersonChat*).
type Options struct {
	DefaultMode     string
	Window          time.Duration
	DevicePerHour   int
	NewPeoplePerDay int
	NoticesPerDay   int
	RepliesPerHour  int
	RepliesPerDay   int
	TenantPerHour   int
	IMTMaxBytes     int
	SBDMaxBytes     int
	// PlatformNumbers are the Hub's own numbers: never a person.
	PlatformNumbers []string
}

// Service runs the lane.
type Service struct {
	bus      bus.MessageBus
	store    Store
	dir      Directory
	tenants  TenantResolver
	accounts Accounts
	toSat    SatSender
	isIMT    func(tenantID, imei string) bool
	opts     Options
	now      func() time.Time
	// later runs the paid part of a webhook's work off its request; tests
	// replace it to run inline.
	later func(ctx context.Context, f func(context.Context))
}

// New builds the lane.
func New(b bus.MessageBus, s Store, dir Directory, tenants TenantResolver, accounts Accounts,
	toSat SatSender, isIMT func(tenantID, imei string) bool, opts Options) *Service {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&opts.DevicePerHour, 20)
	def(&opts.NewPeoplePerDay, 10)
	def(&opts.NoticesPerDay, 5)
	def(&opts.RepliesPerHour, 10)
	def(&opts.RepliesPerDay, 50)
	def(&opts.TenantPerHour, 120)
	def(&opts.IMTMaxBytes, 1024)
	def(&opts.SBDMaxBytes, 270)
	if opts.Window <= 0 {
		opts.Window = 168 * time.Hour
	}
	switch opts.DefaultMode {
	case ModeOff, ModeContacts, ModeOpen:
	default:
		opts.DefaultMode = ModeContacts
	}
	if isIMT == nil {
		isIMT = func(string, string) bool { return false }
	}
	return &Service{bus: b, store: s, dir: dir, tenants: tenants, accounts: accounts,
		toSat: toSat, isIMT: isIMT, opts: opts, now: time.Now, later: detached}
}

// SetDirectory attaches the tenant directory (contacts). Call before Start.
func (s *Service) SetDirectory(d Directory) { s.dir = d }

// detached runs f off the caller's request: an MT can take longer than the
// server's 15 s write timeout, which cuts a slow handler with no response at
// all, and Twilio then retries what was already taken.
func detached(ctx context.Context, f func(context.Context)) {
	bg := context.WithoutCancel(ctx)
	go func() {
		c, cancel := context.WithTimeout(bg, 90*time.Second)
		defer cancel()
		f(c)
	}()
}

// Start subscribes to the Hub's own copy of every satellite MO.
func (s *Service) Start() error {
	for _, f := range hubmqtt.SatMOFilters() {
		if err := s.bus.Subscribe(f, 1, s.handle); err != nil {
			return err
		}
	}
	slog.Info("personchat: lane open", "default_mode", s.opts.DefaultMode, "window", s.opts.Window)
	return nil
}

type decoded struct {
	ID      string              `json:"id"`
	Channel hubmqtt.BearerField `json:"channel"`
	Text    string              `json:"text"`
	Opaque  bool                `json:"opaque"`
	Lane    string              `json:"lane"`
}

// LaneMarker is what the SMS webhook writes into a person's reply that the
// lane took. The reply is still published on mo/decoded, because the SOS
// detector, the dead man's switch and the message history read SMS there and
// an SOS is never dropped; the marker is what tells the routing engine not to
// ALSO relay it.
const LaneMarker = "person"

// addressed is a text from a tenant's own modem to a person.
type addressed struct {
	tenantID string
	device   *store.Device
	target   Target // resolved: KindSMS or KindEmail
	body     string
	msgID    string
}

// classify is the one decision the lane and the routing engine's claim both
// make, on the tenant the topic names and the modem it names.
func (s *Service) classify(ctx context.Context, topicTenant, imei string, payload []byte, fallbackID string) (*addressed, bool) {
	var m decoded
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, false
	}
	if m.Opaque || !isSatellite(string(m.Channel)) || !strings.HasPrefix(strings.TrimSpace(m.Text), "@") {
		return nil, false
	}
	target, body, ok := ParseAddressed(m.Text)
	if !ok || imei == "" || topicTenant == "" {
		return nil, false
	}
	// The topic's tenant must be the modem's registered owner: the resolver
	// answers with the owner whatever namespace a message came in, so a
	// message for another tenant's modem published in this tenant's
	// namespace would otherwise act as that tenant.
	tenantID := s.tenants.ForDeviceTopic(ctx, imei, topicTenant)
	if tenantID != topicTenant {
		return nil, false
	}
	dev, err := s.store.GetDevice(ctx, tenantID, imei)
	if err != nil || dev == nil || dev.IMEI != imei {
		return nil, false
	}
	mode := s.mode(ctx, tenantID)
	if mode == ModeOff {
		return nil, false
	}
	resolved, inDirectory := s.lookup(ctx, tenantID, target)
	switch {
	case target.Kind == KindName && !inDirectory:
		// Not a contact: a mention ("@bob ..."), or APRS's "@CALL text".
		// Ordinary traffic; routes see it.
		return nil, false
	case mode == ModeContacts && !inDirectory:
		// Addressed, but to somebody the tenant never vouched for. Claimed,
		// so no route sends it either, and the device is told why.
		resolved = target
	}
	a := &addressed{tenantID: tenantID, device: dev, target: resolved, body: body, msgID: m.ID}
	if a.msgID == "" {
		a.msgID = fallbackID
	}
	return a, true
}

// Claims tells the routing engine that a decoded message is this lane's to
// deliver, so no route also sends it somewhere: a device's text to a person,
// or a person's reply the SMS webhook marked. It reads mo/decoded, which
// cannot say whether the provider or a bridge published it; the lane SENDS
// only on the provider's copy. So an "@" text a bridge publishes for its own
// tenant's modem reaches neither a route nor a person: a message the
// provider never delivered is not one the Hub relays.
func (s *Service) Claims(topic string, payload []byte) bool {
	var m decoded
	if err := json.Unmarshal(payload, &m); err == nil && m.Lane == LaneMarker && strings.EqualFold(string(m.Channel), "sms") {
		// The SMS webhook's mark. A bridge could put the same field on its
		// own message; all that does is keep routes off that one message.
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, ok := s.classify(ctx, hubmqtt.ExtractTenantID(topic), hubmqtt.ExtractDeviceID(topic), payload, "")
	return ok
}

func (s *Service) mode(ctx context.Context, tenantID string) string {
	t, err := s.store.GetTenant(ctx, tenantID)
	if err != nil || t == nil {
		// A failed lookup is not consent to write to strangers.
		return ModeOff
	}
	switch t.PersonChatMode {
	case ModeOff, ModeContacts, ModeOpen:
		return t.PersonChatMode
	}
	return s.opts.DefaultMode
}

// lookup resolves a target against the tenant's directory: a name to the
// contact's primary SMS or email address, a number or address to whether a
// contact has it. A name two contacts answer to resolves to nobody.
func (s *Service) lookup(ctx context.Context, tenantID string, t Target) (Target, bool) {
	if s.dir == nil {
		return t, false
	}
	contacts, err := s.dir.ListContacts(ctx, tenantID)
	if err != nil {
		return t, false
	}
	if t.Kind == KindName {
		key := NameKey(t.Value)
		var found *directory.Contact
		for i := range contacts {
			if NameKey(contacts[i].DisplayName) == key {
				if found != nil {
					return t, false
				}
				found = &contacts[i]
			}
		}
		if found == nil {
			return t, false
		}
		best, rank := Target{}, int(^uint(0)>>1)
		for _, ad := range found.Addresses {
			if at, ok := addressTarget(ad); ok && ad.PrimaryRank < rank {
				best, rank = at, ad.PrimaryRank
			}
		}
		return best, best.Kind != ""
	}
	for _, c := range contacts {
		for _, ad := range c.Addresses {
			if at, ok := addressTarget(ad); ok && at.Kind == t.Kind && strings.EqualFold(at.Value, t.Value) {
				return t, true
			}
		}
	}
	return t, false
}

func addressTarget(ad directory.Address) (Target, bool) {
	switch ad.Kind {
	case directory.KindSMS, directory.KindCellular:
		if n, ok := E164(strings.TrimSpace(ad.Value)); ok {
			return Target{Kind: KindSMS, Value: n}, true
		}
	case directory.KindEmail:
		if e, ok := EmailAddress(strings.TrimSpace(ad.Value)); ok {
			return Target{Kind: KindEmail, Value: e}, true
		}
	}
	return Target{}, false
}

func (s *Service) handle(topic string, payload []byte) {
	tenantID, imei, ok := hubmqtt.ParseSatMOTopic(topic)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, ok := s.classify(ctx, tenantID, imei, payload, hubmqtt.FallbackMessageID(topic, payload))
	if !ok {
		return
	}
	// One replica sends; the key is the message off the wire.
	won, err := s.store.ClaimOnce(ctx, "personchat:out:"+a.tenantID+":"+a.msgID)
	if err != nil {
		slog.Error("personchat: claim failed, not sending", "error", err)
		return
	}
	if !won {
		return
	}
	if reason := s.send(ctx, a); reason != "" {
		s.notice(ctx, a, reason)
	}
}

// refusal is a reason the device is told, in fixed words: never a provider's
// error text, which can carry anything.
type refusal string

func (r refusal) Error() string { return string(r) }

const (
	reasonLimits     refusal = "the Hub could not check its limits"
	reasonBusy       refusal = "this device has sent its limit for this hour"
	reasonNewPeople  refusal = "this device has started its limit of new conversations for today"
	reasonTenantBusy refusal = "this account has sent its limit for this hour"
	reasonNotContact refusal = "not in this account's contacts (Settings: who devices may write to)"
	reasonNoSMS      refusal = "this account has no SMS provider (Settings, Integrations)"
	reasonNoMail     refusal = "this account has no mail gateway (Settings, Integrations)"
	reasonSMSLong    refusal = "an encrypted SMS must fit one message"
	reasonRecord     refusal = "the Hub could not record the conversation"
	reasonProvider   refusal = "the provider refused it"
	reasonOwnNumber  refusal = "that is one of the Hub's or this account's own numbers"
	reasonKitSIM     refusal = "that number is a kit's SIM; write to the kit on its mesh"
	reasonOwnAddress refusal = "that is this account's own address"
)

// send delivers a device's text to the person, and answers why not.
func (s *Service) send(ctx context.Context, a *addressed) refusal {
	if s.mode(ctx, a.tenantID) == ModeContacts {
		if _, ok := s.lookup(ctx, a.tenantID, a.target); !ok {
			return reasonNotContact
		}
	}
	if r := s.refuseTarget(ctx, a.tenantID, a.target); r != "" {
		return r
	}
	now := s.now().UTC()
	hour := now.Format("2006010215")
	if ok, err := s.slot(ctx, a.tenantID, "dev:"+a.device.IMEI+":"+hour, s.opts.DevicePerHour); err != nil {
		return reasonLimits
	} else if !ok {
		return reasonBusy
	}
	if ok, err := s.slot(ctx, a.tenantID, "out:"+hour, s.opts.TenantPerHour); err != nil {
		return reasonLimits
	} else if !ok {
		return reasonTenantBusy
	}
	conv, err := s.store.SavePersonConversationOut(ctx, &store.PersonConversation{
		TenantID: a.tenantID, ID: conversationID(a.tenantID, a.device.IMEI, a.target),
		DeviceID: a.device.IMEI, DeviceKind: "satellite", PeerKind: string(a.target.Kind), Peer: a.target.Value,
		Tag: deviceTag(a.tenantID, a.device.IMEI), Ref: newRef(),
	})
	if err != nil {
		slog.Error("personchat: saving the conversation failed", "error", err)
		return reasonRecord
	}
	if conv.LastOutAt.IsZero() {
		if ok, err := s.slot(ctx, a.tenantID, "new:"+a.device.IMEI+":"+now.Format("20060102"), s.opts.NewPeoplePerDay); err != nil {
			return reasonLimits
		} else if !ok {
			return reasonNewPeople
		}
	}
	label := deviceLabel(a.device)
	var e2e bool
	var mailID string
	var sendErr error
	switch a.target.Kind {
	case KindSMS:
		e2e, sendErr = s.sendSMS(ctx, a, conv, label)
	case KindEmail:
		e2e, mailID, sendErr = s.sendMail(ctx, a, conv, label)
	}
	status := "sent"
	if sendErr != nil {
		status = "failed"
	}
	_ = s.store.RecordPersonChatSend(ctx, &store.PersonChatSend{TenantID: a.tenantID, ID: "out:" + digest(a.msgID),
		ConversationID: conv.ID, Direction: "out", E2E: e2e, Bytes: len(a.body), Status: status})
	if sendErr != nil {
		slog.Warn("personchat: device -> person failed", "tenant", a.tenantID, "kind", a.target.Kind,
			"to", suffix(a.target.Value), "error", sendErr)
		var r refusal
		if errors.As(sendErr, &r) {
			return r
		}
		return reasonProvider
	}
	// Only an accepted send opens (or extends) the window for replies.
	if err := s.store.MarkPersonConversationOut(ctx, a.tenantID, conv.ID, now, mailID); err != nil {
		slog.Error("personchat: recording the send failed; replies may not reach the device", "error", err)
	}
	slog.Info("personchat: device -> person", "tenant", a.tenantID, "kind", a.target.Kind,
		"to", suffix(a.target.Value), "e2e", e2e, "conversation", conv.ID)
	return ""
}

// refuseTarget keeps the lane from writing to itself: the Hub's numbers, the
// tenant's own sending number and addresses, a kit's SIM (whose texts are kit
// traffic for the kit's lanes, and come back in as a "reply").
func (s *Service) refuseTarget(ctx context.Context, tenantID string, t Target) refusal {
	switch t.Kind {
	case KindSMS:
		for _, n := range s.opts.PlatformNumbers {
			if n != "" && n == t.Value {
				return reasonOwnNumber
			}
		}
		if sms := s.accounts.SMS(ctx, tenantID); sms != nil && sms.From() == t.Value {
			return reasonOwnNumber
		}
		bridges, err := s.store.ListBridges(ctx, tenantID)
		if err != nil {
			return reasonLimits
		}
		for _, b := range bridges {
			if b == nil {
				continue
			}
			p, err := s.store.GetOOBPeer(ctx, tenantID, b.BridgeID)
			if errors.Is(err, sql.ErrNoRows) {
				continue // a kit with no SIM on file
			}
			if err != nil {
				return reasonLimits
			}
			if p != nil && p.Phone == t.Value {
				return reasonKitSIM
			}
		}
	case KindEmail:
		if s.ownAddress(ctx, tenantID, t.Value) {
			return reasonOwnAddress
		}
	}
	return ""
}

func (s *Service) ownAddress(ctx context.Context, tenantID, addr string) bool {
	m, replyTo := s.accounts.Mail(ctx, tenantID)
	if m == nil {
		return false
	}
	own, _ := NormaliseEmail(m.From())
	rt, _ := NormaliseEmail(replyTo)
	return (own != "" && strings.EqualFold(own, addr)) || (rt != "" && strings.EqualFold(rt, addr))
}

func (s *Service) sendSMS(ctx context.Context, a *addressed, conv *store.PersonConversation, label string) (bool, error) {
	sms := s.accounts.SMS(ctx, a.tenantID)
	if sms == nil {
		return false, reasonNoSMS
	}
	if SealedSMS(a.body) {
		body := strings.TrimSpace(a.body)
		if len(body) > 160 {
			return true, reasonSMSLong
		}
		return true, sms.Send(ctx, a.target.Value, body)
	}
	head := label + ": "
	if open := s.openWith(ctx, a.tenantID, KindSMS, a.target.Value); len(open) > 1 ||
		(len(open) == 1 && open[0].ID != conv.ID) {
		// Several of this tenant's devices are talking to this person: say
		// which one this is, and the person starts a reply with it.
		head = label + " " + conv.Tag + ": "
	}
	return false, sms.Send(ctx, a.target.Value, satchat.FitSMS(head, Printable(a.body)))
}

func (s *Service) sendMail(ctx context.Context, a *addressed, conv *store.PersonConversation, label string) (bool, string, error) {
	m, replyTo := s.accounts.Mail(ctx, a.tenantID)
	if m == nil {
		return false, "", reasonNoMail
	}
	subject := fmt.Sprintf("%s via MeshSat [ref %s]", headerSafe(label), conv.Ref)
	footer := fmt.Sprintf("\n\n-- \nSent by %s over satellite through MeshSat.\n"+
		"Reply to this email to answer; keep [ref %s] in the subject.\n"+
		"Replies go to the device over a satellite link that carries about %d bytes, so keep them short.",
		label, conv.Ref, s.mtLimit(a.tenantID, a.device.IMEI))
	msg := email.Message{To: a.target.Value, Subject: subject, ReplyTo: replyTo, RequireTLS: true}
	if conv.LastInMessageID != "" {
		msg.InReplyTo, msg.References = conv.LastInMessageID, conv.LastInMessageID
	}
	e2e := false
	if armoured, ok := PGPMessage(a.body); ok {
		e2e = true
		msg.Body, msg.AsIs = armoured+footer, true
	} else {
		msg.Body = Printable(a.body) + footer
	}
	id, err := m.SendMessage(ctx, msg)
	return e2e, id, err
}

// notice tells the device, once per message and a few times a day at most,
// that its text was not sent. Silence would leave the person in the field
// believing it went.
func (s *Service) notice(ctx context.Context, a *addressed, reason refusal) {
	won, err := s.store.ClaimOnce(ctx, "personchat:notice:"+a.tenantID+":"+a.msgID)
	if err != nil || !won {
		return
	}
	day := s.now().UTC().Format("20060102")
	if ok, err := s.slot(ctx, a.tenantID, "notices:"+a.device.IMEI+":"+day, s.opts.NoticesPerDay); err != nil || !ok {
		slog.Info("personchat: not telling the device again today", "tenant", a.tenantID, "reason", string(reason))
		return
	}
	text := Cut(fmt.Sprintf("[MeshSat Hub] not sent to %s: %s", a.target.Value, reason), s.mtLimit(a.tenantID, a.device.IMEI))
	err = s.toSat(ctx, a.tenantID, a.device.IMEI, text)
	status := "sent"
	if err != nil {
		status = "failed"
		slog.Error("personchat: notice to the device failed", "tenant", a.tenantID, "device", suffix(a.device.IMEI), "error", err)
	}
	_ = s.store.RecordPersonChatSend(ctx, &store.PersonChatSend{TenantID: a.tenantID, ID: "notice:" + digest(a.msgID),
		ConversationID: conversationID(a.tenantID, a.device.IMEI, a.target), Direction: "notice", Bytes: len(text), Status: status})
	slog.Info("personchat: device told its text was not sent", "tenant", a.tenantID, "reason", string(reason))
}

func (s *Service) mtLimit(tenantID, imei string) int {
	if s.isIMT(tenantID, imei) {
		return s.opts.IMTMaxBytes
	}
	return s.opts.SBDMaxBytes
}

// slot counts one use of a ceiling in the tenant's bucket for key: one
// write, and it holds across both replicas with no count-then-insert race.
// The buckets are clock windows (the hour, the UTC day), not rolling ones.
func (s *Service) slot(ctx context.Context, tenantID, key string, max int) (bool, error) {
	return s.store.TakePersonChatSlot(ctx, tenantID, key, max)
}

// openWith is the person's conversations in the tenant that are open: the
// device's last accepted send is inside the window.
func (s *Service) openWith(ctx context.Context, tenantID string, kind Kind, peer string) []store.PersonConversation {
	open, err := s.store.PersonConversationsWith(ctx, tenantID, string(kind), peer, s.now().Add(-s.opts.Window))
	if err != nil {
		return nil
	}
	out := open[:0]
	for _, c := range open {
		if !c.LastOutAt.IsZero() {
			out = append(out, c)
		}
	}
	return out
}

// ---- person -> device ----

// InboundSMS is what the SMS webhook hands the lane.
type InboundSMS struct {
	From       string // E.164
	MessageSID string // Twilio's; the lane takes no SMS without one
	// Body is the SMS exactly as it arrived. Sealed: a MeshSat frame no Hub
	// key opened, end to end, carried as Body. Opened: a frame a Hub key
	// opened, which is MeshSat app traffic for the pipeline, never a reply.
	Body   string
	Sealed bool
	Opened bool
}

// TakeSMS hands a person's SMS to its conversation. It answers true when the
// SMS was the lane's (delivered, refused or held for a hint): the webhook
// still publishes it, marked, so SOS detection and the history see it.
func (s *Service) TakeSMS(ctx context.Context, tenantID string, in InboundSMS) bool {
	from, ok := E164(strings.TrimSpace(in.From))
	if !ok || in.Opened {
		return false
	}
	// A Twilio delivery always carries a MessageSid. A request without one
	// came by the weaker custom-relay signature, which covers neither the
	// SID nor a time and can be replayed: never a paid MT.
	if strings.TrimSpace(in.MessageSID) == "" {
		return false
	}
	all, err := s.store.PersonConversationsWith(ctx, tenantID, string(KindSMS), from, time.Time{})
	if err != nil || len(all) == 0 {
		return false
	}
	open := s.openWith(ctx, tenantID, KindSMS, from)
	if len(open) == 0 {
		// Somebody the tenant's devices once wrote to, outside the window.
		// Not the lane's: the SMS goes through the pipeline as before.
		return false
	}
	won, err := s.store.ClaimOnce(ctx, "personchat:in:"+tenantID+":"+in.MessageSID)
	if err != nil {
		// Not taken: the SMS goes through the pipeline as it did before the
		// lane, rather than being marked for a lane that will not deliver it.
		slog.Error("personchat: claim failed, leaving the SMS to the pipeline", "error", err)
		return false
	}
	if !won {
		return true
	}
	e2e := in.Sealed && SealedSMS(in.Body)
	text := strings.TrimSpace(in.Body)
	conv, rest, decided := pickByTag(all, open, text, e2e)
	if conv == nil {
		if !decided {
			s.later(ctx, func(ctx context.Context) { s.hint(ctx, tenantID, from, open) })
		} else {
			slog.Info("personchat: a reply names a closed or ambiguous conversation, not delivered", "tenant", tenantID)
		}
		return true
	}
	body := rest
	if !e2e {
		body = flatten(Printable(rest))
		if body == "" {
			return true
		}
	}
	s.later(ctx, func(ctx context.Context) {
		s.deliver(ctx, tenantID, conv, "sms:"+in.MessageSID, from, body, e2e, "")
	})
	return true
}

var tagPattern = regexp.MustCompile(`^([A-Za-z][2-9][A-Za-z])[\s:]+`)

// pickByTag chooses the conversation a reply belongs to. A reply that starts
// with a conversation tag goes to that conversation if it is open, and to
// NOBODY if the tag is a closed one's or two share it: falling through to
// "the only open one" would hand it to a different device. A reply with no
// tag goes to the only open conversation; with several open, nobody, and
// decided is false so the person is told how to choose.
func pickByTag(all, open []store.PersonConversation, text string, e2e bool) (conv *store.PersonConversation, rest string, decided bool) {
	if !e2e {
		if m := tagPattern.FindStringSubmatch(text); m != nil {
			tag := strings.ToUpper(m[1])
			known := false
			for _, c := range all {
				if c.Tag == tag {
					known = true
				}
			}
			if known {
				var hit *store.PersonConversation
				for i := range open {
					if open[i].Tag == tag {
						if hit != nil {
							return nil, "", true
						}
						hit = &open[i]
					}
				}
				if hit == nil {
					return nil, "", true
				}
				return hit, strings.TrimSpace(text[len(m[0]):]), true
			}
		}
	}
	if len(open) == 1 {
		return &open[0], text, true
	}
	return nil, "", false
}

// hint tells a person with several open conversations how to pick one, once
// a day.
func (s *Service) hint(ctx context.Context, tenantID, from string, open []store.PersonConversation) {
	day := s.now().UTC().Format("20060102")
	won, err := s.store.ClaimOnce(ctx, "personchat:hint:"+tenantID+":"+digest(from)+":"+day)
	if err != nil || !won {
		return
	}
	sms := s.accounts.SMS(ctx, tenantID)
	if sms == nil {
		return
	}
	tags := make([]string, 0, len(open))
	for _, c := range open {
		tags = append(tags, c.Tag)
	}
	text := satchat.FitSMS("", "MeshSat: several devices wrote to you. Start your reply with "+strings.Join(tags, " or ")+" to say which one.")
	if err := sms.Send(ctx, from, text); err != nil {
		slog.Warn("personchat: the hint SMS failed", "error", err)
	}
}

var refPattern = regexp.MustCompile(`(?i)\[ref ([A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4})\]`)

// TakeMailReply hands a person's mail reply to its conversation. A reply is
// the lane's only when its SUBJECT carries the reference of a conversation of
// this tenant AND it comes from the address that conversation wrote to, and
// the mail service's domain checks did not fail. The reference is what
// authorises it: 60 random bits that only the mailbox the Hub wrote to has
// (and whoever that mailbox forwards the mail to). The From check is a second
// lock, not a strong one, since a From line is forged easily where the
// sender's domain publishes no DMARC policy. Never read from the body, where
// a forwarded mail quotes it.
func (s *Service) TakeMailReply(ctx context.Context, tenantID string, in email.InboundReply) bool {
	from, ok := NormaliseEmail(in.From)
	if !ok {
		return false
	}
	m := refPattern.FindStringSubmatch(in.Subject)
	if m == nil {
		return false
	}
	conv, err := s.store.GetPersonConversationByRef(ctx, tenantID, strings.ToUpper(m[1]))
	if err != nil || conv == nil || conv.PeerKind != string(KindEmail) {
		return false
	}
	switch {
	case !strings.EqualFold(conv.Peer, from):
		slog.Warn("personchat: a reply with a conversation's reference from another address, not delivered",
			"tenant", tenantID, "conversation", conv.ID)
		return true
	case s.ownAddress(ctx, tenantID, from):
		return true
	case in.AutoReply:
		slog.Info("personchat: an automatic reply, not sent to the device", "tenant", tenantID, "conversation", conv.ID)
		return true
	case in.AuthFailed:
		slog.Warn("personchat: the sender's domain failed the mail service's checks, not delivered",
			"tenant", tenantID, "conversation", conv.ID)
		return true
	case conv.LastOutAt.IsZero() || conv.LastOutAt.Before(s.now().Add(-s.opts.Window)):
		slog.Info("personchat: a reply after the window closed, not delivered", "tenant", tenantID, "conversation", conv.ID)
		return true
	}
	key := strings.TrimSpace(in.MessageID)
	if key == "" {
		key = digest(from, in.Subject, in.Text, in.PGP)
	}
	won, err := s.store.ClaimOnce(ctx, "personchat:in:"+tenantID+":mail:"+digest(key))
	if err != nil {
		slog.Error("personchat: claim failed, mail reply not delivered", "error", err)
		return false
	}
	if !won {
		return true
	}
	var body string
	e2e := false
	if in.PGP != "" {
		if compact, ok := CompactPGP(in.PGP); ok {
			body, e2e = compact, true
		}
	}
	if !e2e {
		body = flatten(Printable(ReplyText(in.Text)))
		if body == "" {
			return true
		}
	}
	msgID := cleanMessageID(in.MessageID)
	s.later(ctx, func(ctx context.Context) {
		s.deliver(ctx, tenantID, conv, "mail:"+digest(key), from, body, e2e, msgID)
	})
	return true
}

// deliver sends a person's reply to the device, inside the gates. Every
// reply is a paid MT on the tenant's account.
func (s *Service) deliver(ctx context.Context, tenantID string, conv *store.PersonConversation, msgID, from, body string, e2e bool, mailID string) {
	// The modem must still be this tenant's: a deleted or transferred device
	// would otherwise be messaged on whatever account owns it now. Read from
	// the store, not a cache.
	if dev, err := s.store.GetDevice(ctx, tenantID, conv.DeviceID); err != nil || dev == nil {
		slog.Warn("personchat: the device is no longer this tenant's, reply not sent", "tenant", tenantID, "conversation", conv.ID)
		return
	}
	now := s.now().UTC()
	if over := s.overQuota(ctx, tenantID, conv.ID, now); over != "" {
		slog.Warn("personchat: reply over the limit, not sent to the device", "tenant", tenantID, "conversation", conv.ID, "limit", over)
		s.tellPerson(ctx, tenantID, conv, "personchat:quota:", "MeshSat: the satellite link to this device takes "+over+"; your reply was not sent.")
		return
	}
	head := "[from " + from + "] "
	limit := s.mtLimit(tenantID, conv.DeviceID)
	var text string
	if e2e {
		text = head + body
		if len(text) > limit {
			slog.Warn("personchat: an end-to-end reply too long for the device's link, not sent", "tenant", tenantID, "bytes", len(text), "limit", limit)
			s.tellPerson(ctx, tenantID, conv, "personchat:toolong:", fmt.Sprintf("MeshSat: your encrypted reply is %d bytes; the satellite link to this device takes %d. It was not sent.", len(text), limit))
			return
		}
	} else {
		text = head + Cut(body, limit-len(head))
	}
	err := s.toSat(ctx, tenantID, conv.DeviceID, text)
	status := "sent"
	if err != nil {
		status = "failed"
	}
	_ = s.store.RecordPersonChatSend(ctx, &store.PersonChatSend{TenantID: tenantID, ID: "in:" + digest(msgID),
		ConversationID: conv.ID, Direction: "in", E2E: e2e, Bytes: len(text), Status: status})
	if err != nil {
		slog.Error("personchat: person -> device failed", "tenant", tenantID, "device", suffix(conv.DeviceID), "error", err)
		return
	}
	_ = s.store.MarkPersonConversationIn(ctx, tenantID, conv.ID, now, mailID)
	slog.Info("personchat: person -> device", "tenant", tenantID, "conversation", conv.ID, "e2e", e2e, "bytes", len(text))
}

func (s *Service) overQuota(ctx context.Context, tenantID, convID string, now time.Time) string {
	hour, day := now.Format("2006010215"), now.Format("20060102")
	checks := []struct {
		key string
		max int
		say string
	}{
		{"rh:" + convID + ":" + hour, s.opts.RepliesPerHour, fmt.Sprintf("%d replies an hour", s.opts.RepliesPerHour)},
		{"rd:" + convID + ":" + day, s.opts.RepliesPerDay, fmt.Sprintf("%d replies a day", s.opts.RepliesPerDay)},
		{"in:" + hour, s.opts.TenantPerHour, fmt.Sprintf("%d replies an hour for the whole account", s.opts.TenantPerHour)},
	}
	for _, c := range checks {
		ok, err := s.slot(ctx, tenantID, c.key, c.max)
		if err != nil {
			return "no replies while the Hub cannot check its limits"
		}
		if !ok {
			return c.say
		}
	}
	return ""
}

// tellPerson answers the person on the bearer they wrote on, once a day per
// conversation and reason.
func (s *Service) tellPerson(ctx context.Context, tenantID string, conv *store.PersonConversation, prefix, text string) {
	won, err := s.store.ClaimOnce(ctx, prefix+tenantID+":"+conv.ID+":"+s.now().UTC().Format("20060102"))
	if err != nil || !won {
		return
	}
	switch conv.PeerKind {
	case string(KindSMS):
		if sms := s.accounts.SMS(ctx, tenantID); sms != nil {
			_ = sms.Send(ctx, conv.Peer, satchat.FitSMS("", text))
		}
	case string(KindEmail):
		if m, replyTo := s.accounts.Mail(ctx, tenantID); m != nil {
			_, _ = m.SendMessage(ctx, email.Message{To: conv.Peer, ReplyTo: replyTo, RequireTLS: true,
				Subject: "MeshSat [ref " + conv.Ref + "]", Body: text, InReplyTo: conv.LastInMessageID})
		}
	}
}

var messageIDPattern = regexp.MustCompile(`^<[!-;=?-~]{1,250}>$`)

// cleanMessageID keeps an inbound Message-ID only when it has the RFC 5322
// shape: it goes into the next mail's In-Reply-To and References headers.
func cleanMessageID(id string) string {
	id = strings.TrimSpace(id)
	if !messageIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// ---- identities ----

// conversationID is the same on both replicas and on every write to the same
// person: tenant, device, bearer and person.
func conversationID(tenantID, imei string, t Target) string {
	return "pc_" + digest(tenantID, imei, string(t.Kind), strings.ToLower(t.Value))[:24]
}

// tagLetters and tagDigits are satchat's: nothing easily misread on a phone.
const (
	tagLetters = "ABCDEFGHJKMNPQRSTVWXYZ"
	tagDigits  = "23456789"
)

// deviceTag is a device's three-character name in a person's SMS when
// several of the tenant's devices write to them ("K7Q"). Derived, so it is
// the same on both replicas and in every conversation of that device; with
// 3872 of them, two devices writing to one person share a tag about as often
// as never, and when they do a tagged reply is held, not guessed.
func deviceTag(tenantID, imei string) string {
	sum := sha256.Sum256([]byte("meshsat-personchat|" + tenantID + "|" + imei))
	return string([]byte{
		tagLetters[int(sum[0])%len(tagLetters)],
		tagDigits[int(sum[1])%len(tagDigits)],
		tagLetters[int(sum[2])%len(tagLetters)],
	})
}

// newRef is an email conversation's reference: 60 random bits, written
// XXXX-XXXX-XXXX in the subject.
func newRef() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])[:12]
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12]
}

func deviceLabel(d *store.Device) string {
	l := headerSafe(Printable(strings.TrimSpace(d.Label)))
	if l == "" {
		l = "MeshSat device " + suffix(d.IMEI)
	}
	if utf8.RuneCountInString(l) > 32 {
		l = string([]rune(l)[:32])
	}
	return l
}

func headerSafe(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)), " ")
}

// flatten turns line breaks into spaces: a reply shown on a device must not
// be able to start a line that reads as the Hub's own "[from ...]" or
// "[MeshSat Hub]".
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func isSatellite(channel string) bool {
	switch strings.ToLower(channel) {
	case "iridium", "iridium_imt", "iridium_sbd", "satellite", "globalstar":
		return true
	}
	return false
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func suffix(id string) string {
	if len(id) <= 4 {
		return id
	}
	return "…" + id[len(id)-4:]
}
