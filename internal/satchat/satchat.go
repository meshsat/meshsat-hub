// Package satchat is a two-way text lane between a roaming satellite node and
// the field kits' meshes (MESHSAT-1290).
//
// The case it exists for: the kits are indoors where their own satellite modems
// see no sky, and somebody walks outside with a handheld node and a RockBLOCK.
//
//	street -> stand: the node's MO reaches the Hub through the provider. The Hub
//	texts every kit "*K7 hello from outside"; a kit puts an inbound SMS on its
//	mesh verbatim, so that is what the handhelds show.
//
//	stand -> street: somebody answers on the mesh STARTING with the token,
//	"*K7 on my way". The kit gets that to the Hub (by SMS on its Hub lane, or
//	over MQTT when it has internet, or both), and the Hub sends the text to the
//	node's modem as an MT.
//
// The first character picks the lane: "*" is this one, "#" is the visitor SMS
// chat (internal/booth), anything else is ordinary traffic and none of this
// code looks at it.
//
// # Why the token is derived and not stored
//
// A token only has to answer "which modem is this reply for". Deriving it from
// the modem's IMEI makes it the same on both replicas and across restarts with
// no table, no expiry and nothing to clean up; a conversation that goes quiet
// for an hour still works when the answer finally comes. Correlation is by the
// token in the BODY, never by (kit, time).
//
// # Why both legs hang off mo/decoded
//
// Everything that reaches the Hub, by any bearer, comes out of its own decode
// chain as one mo/decoded message. Listening there means a kit's reply is read
// AFTER decryption and decompression whichever way it travelled, and the
// webhooks, the routing engine and the "#" chat are not touched at all.
package satchat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Kit is one field kit the lane delivers to and accepts replies from.
type Kit struct {
	BridgeID string
	Label    string
}

// KitSender puts a text on a kit over that kit's SIM.
type KitSender interface {
	SendToKit(ctx context.Context, tenantID, bridgeID, body string) error
}

// SatSender sends plain text to one satellite modem as an MT.
type SatSender func(ctx context.Context, tenantID, imei, text string) error

// Store is the slice of store.Store the lane needs.
type Store interface {
	ClaimOnce(ctx context.Context, key string) (bool, error)
	GetOOBPeer(ctx context.Context, tenantID, bridgeID string) (*store.OOBPeer, error)
}

// TenantResolver answers which tenant a device's message belongs to.
type TenantResolver interface {
	ForDeviceTopic(ctx context.Context, imei, topicTenant string) string
}

// Options tune the lane. Zero values take the defaults.
type Options struct {
	// MaxPerHour caps street -> stand messages per replica. The once-only
	// claims already stop duplicates; this stops a stuck sender from emptying
	// the kits' prepaid bundles, two texts at a time.
	MaxPerHour int
	// MTMaxBytes is the most text sent down to a modem. An SBD MT frame is 270
	// bytes and the version byte and any compression are the sender's business.
	MTMaxBytes int
}

// Service runs the lane.
type Service struct {
	bus     bus.MessageBus
	store   Store
	tenants TenantResolver
	kits    []Kit
	toKit   KitSender
	toSat   SatSender
	tokens  map[string]string // token -> imei
	byIMEI  map[string]string // imei -> token
	opts    Options
	now     func() time.Time

	mu   sync.Mutex
	sent []time.Time
}

// New builds the lane for the given satellite devices (IMEIs) and kits.
func New(b bus.MessageBus, s Store, tenants TenantResolver, devices []string, kits []Kit, toKit KitSender, toSat SatSender, opts Options) *Service {
	if opts.MaxPerHour <= 0 {
		opts.MaxPerHour = 60
	}
	if opts.MTMaxBytes <= 0 {
		opts.MTMaxBytes = 250
	}
	svc := &Service{bus: b, store: s, tenants: tenants, kits: kits, toKit: toKit, toSat: toSat, opts: opts, now: time.Now}
	svc.tokens, svc.byIMEI = Tokens(devices)
	return svc
}

// tokenLetters and tokenDigits leave out what is easily mistyped or misread on
// a handheld keyboard (I L O U, 0 1). Every character is in the GSM 03.38 basic
// table, so a token survives a text-mode SMS unchanged.
const (
	tokenLetters = "ABCDEFGHJKMNPQRSTVWXYZ"
	tokenDigits  = "23456789"
)

// Tokens derives one "letter digit" token per IMEI. It is deterministic: the
// same list gives the same tokens on every replica and after every restart. A
// collision walks on through the hash, in sorted IMEI order, so the outcome
// does not depend on the order the list was written in.
func Tokens(imeis []string) (byToken, byIMEI map[string]string) {
	byToken, byIMEI = map[string]string{}, map[string]string{}
	sorted := append([]string(nil), imeis...)
	sort.Strings(sorted)
	for _, imei := range sorted {
		imei = strings.TrimSpace(imei)
		if imei == "" || byIMEI[imei] != "" {
			continue
		}
		sum := sha256.Sum256([]byte("meshsat-satchat|" + imei))
		for i := 0; i+1 < len(sum); i++ {
			tok := string([]byte{tokenLetters[int(sum[i])%len(tokenLetters)], tokenDigits[int(sum[i+1])%len(tokenDigits)]})
			if _, taken := byToken[tok]; !taken {
				byToken[tok], byIMEI[imei] = imei, tok
				break
			}
		}
	}
	return byToken, byIMEI
}

// Start subscribes to the decoded-message stream.
func (s *Service) Start() error {
	for _, f := range hubmqtt.DualFilters("meshsat/+/mo/decoded") {
		if err := s.bus.Subscribe(f, 1, s.handle); err != nil {
			return err
		}
	}
	for imei, tok := range s.byIMEI {
		slog.Info("satchat: lane open", "token", "*"+tok, "device_suffix", suffix(imei), "kits", len(s.kits))
	}
	return nil
}

type decoded struct {
	ID       string          `json:"id"`
	IMEI     string          `json:"imei"`
	DeviceID string          `json:"device_id"`
	BridgeID string          `json:"bridge_id"`
	Channel  json.RawMessage `json:"channel"` // a bearer name, or a mesh channel index from the Bridge
	Text     string          `json:"text"`
	Opaque   bool            `json:"opaque"`
}

func (s *Service) handle(topic string, payload []byte) {
	var m decoded
	if err := json.Unmarshal(payload, &m); err != nil {
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" || m.Opaque {
		return
	}
	origin := hubmqtt.ExtractDeviceID(topic)
	if origin == "" {
		return
	}
	msgID := m.ID
	if msgID == "" {
		msgID = hubmqtt.FallbackMessageID(topic, payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tenantID := s.tenants.ForDeviceTopic(ctx, origin, hubmqtt.ExtractTenantID(topic))

	// street -> stand: a readable satellite text from a lane device.
	if tok, ok := s.byIMEI[origin]; ok {
		if isSatellite(m.Channel) {
			s.toKits(ctx, tenantID, origin, tok, msgID, text)
		}
		return
	}

	// stand -> street: a text that starts with "*", from one of the kits.
	tok, body, ok := ParseReply(text)
	if !ok {
		return
	}
	kit, ok := s.kitFor(ctx, tenantID, origin, m.BridgeID)
	if !ok {
		// Not a kit. Anybody can text the Hub's number; only a kit may spend
		// the owner's satellite credit.
		slog.Warn("satchat: a '*' reply from something that is not a kit, ignored", "origin_suffix", suffix(origin))
		return
	}
	s.toSatellite(ctx, tenantID, kit, tok, body)
}

// toKits texts every kit, once each across the replicas.
func (s *Service) toKits(ctx context.Context, tenantID, imei, tok, msgID, text string) {
	if !s.withinRate() {
		slog.Warn("satchat: hourly ceiling reached, not texting the kits", "max_per_hour", s.opts.MaxPerHour)
		return
	}
	body := FitSMS("*"+tok+" ", text)
	for _, k := range s.kits {
		won, err := s.store.ClaimOnce(ctx, "satchat:sms:"+tenantID+":"+msgID+":"+k.BridgeID)
		if err != nil {
			slog.Error("satchat: claim failed, not sending", "kit", k.BridgeID, "error", err)
			continue
		}
		if !won {
			continue // the other replica, or a provider redelivery
		}
		// Remember what went onto that mesh, so the same words coming back
		// from a kit are recognised as an echo and not sent up to the modem.
		_, _ = s.store.ClaimOnce(ctx, echoKey(tenantID, imei, text, s.now()))
		if err := s.toKit.SendToKit(ctx, tenantID, k.BridgeID, body); err != nil {
			slog.Error("satchat: SMS to kit failed", "kit", k.BridgeID, "error", err)
			continue
		}
		slog.Info("satchat: street -> stand", "token", "*"+tok, "kit", k.BridgeID, "chars", utf8.RuneCountInString(body))
	}
}

// toSatellite sends a kit's reply down to the modem its token names.
func (s *Service) toSatellite(ctx context.Context, tenantID string, kit Kit, tok, body string) {
	imei, known := s.tokens[tok]
	if !known {
		// A mistyped or stale token. With one satellite node in the lane there
		// is no doubt who is meant; with several, guessing would send somebody
		// a message meant for somebody else.
		if len(s.byIMEI) != 1 {
			slog.Warn("satchat: reply carries an unknown token and the lane has several devices, dropped", "token", "*"+tok)
			return
		}
		for only := range s.byIMEI {
			imei = only
		}
		slog.Info("satchat: unknown token, using the only device in the lane", "token", "*"+tok)
	}
	now := s.now()
	// The same reply can arrive twice by design: by SMS on the kit's Hub lane
	// AND over MQTT when the kit has internet. One MT, whichever came first.
	// Buckets of ten minutes, this one and the one before, so a pair that
	// straddles a boundary is still one message and the same words an hour
	// later are a new one.
	for _, at := range []time.Time{now.Add(-10 * time.Minute), now} {
		won, err := s.store.ClaimOnce(ctx, replyKey(tenantID, imei, body, at))
		if err != nil {
			slog.Error("satchat: claim failed, not sending", "error", err)
			return
		}
		if !won {
			return
		}
	}
	// An echo of what the Hub itself just put on that mesh is not a reply.
	for _, at := range []time.Time{now.Add(-10 * time.Minute), now} {
		if won, err := s.store.ClaimOnce(ctx, echoKey(tenantID, imei, body, at)); err == nil && !won {
			slog.Info("satchat: a kit echoed the Hub's own message, not sent to the modem", "kit", kit.BridgeID)
			return
		}
	}
	out := body
	if kit.Label != "" {
		out = kit.Label + ": " + body
	}
	out = truncateBytes(out, s.opts.MTMaxBytes)
	if err := s.toSat(ctx, tenantID, imei, out); err != nil {
		slog.Error("satchat: MT to the modem failed", "device_suffix", suffix(imei), "error", err)
		return
	}
	slog.Info("satchat: stand -> street", "token", "*"+tok, "kit", kit.BridgeID, "bytes", len(out))
}

// kitFor answers whether origin is one of the lane's kits: by its bridge id when
// the reply came over MQTT, by its SIM number when it came by SMS.
func (s *Service) kitFor(ctx context.Context, tenantID, origin, bridgeID string) (Kit, bool) {
	for _, k := range s.kits {
		if bridgeID != "" && bridgeID == k.BridgeID {
			return k, true
		}
	}
	if !strings.HasPrefix(origin, "+") {
		return Kit{}, false
	}
	for _, k := range s.kits {
		peer, err := s.store.GetOOBPeer(ctx, tenantID, k.BridgeID)
		if err == nil && peer != nil && peer.Phone != "" && peer.Phone == origin {
			return k, true
		}
	}
	return Kit{}, false
}

func (s *Service) withinRate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	keep := s.sent[:0]
	for _, t := range s.sent {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	s.sent = keep
	if len(s.sent) >= s.opts.MaxPerHour {
		return false
	}
	s.sent = append(s.sent, now)
	return true
}

// ParseReply splits "*K7 on my way" into the token and the text. It tolerates
// what a handheld keyboard and a hurried thumb produce: lower case, no space
// after the token, a space after the star, a colon.
func ParseReply(text string) (token, body string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "*") {
		return "", "", false
	}
	t = strings.TrimSpace(t[1:])
	if len(t) < 3 {
		return "", "", false
	}
	tok := strings.ToUpper(t[:2])
	if !strings.ContainsRune(tokenLetters+"ILOU", rune(tok[0])) || tok[1] < '0' || tok[1] > '9' {
		return "", "", false
	}
	body = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(t[2:]), ":-"))
	if body == "" {
		return "", "", false
	}
	return tok, body, true
}

// gsmExtended are the GSM 03.38 extension-table characters: each costs two
// septets in a text message.
const gsmExtended = "^{}\\[~]|€"

// FitSMS returns head+text cut to ONE message segment. It matters more than it
// looks: from the Hub's number a multi-segment message is accepted by the
// carrier and then silently never delivered, and a single character outside
// the GSM alphabet switches the whole message to UCS-2 and drops the limit
// from 160 to 70.
func FitSMS(head, text string) string {
	full := head + text
	limit := 160
	for _, r := range full {
		if r > 127 && !strings.ContainsRune("£¥èéùìòÇØøÅåΔΦΓΛΩΠΨΣΘΞÆæßÉÄÖÑÜ§¿äöñüà¡¤", r) {
			limit = 70
			break
		}
	}
	used, out := 0, make([]rune, 0, len(full))
	for _, r := range full {
		cost := 1
		if limit == 160 && strings.ContainsRune(gsmExtended, r) {
			cost = 2
		}
		if used+cost > limit {
			break
		}
		used += cost
		out = append(out, r)
	}
	return string(out)
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func isSatellite(raw json.RawMessage) bool {
	var name string
	if len(raw) == 0 || json.Unmarshal(raw, &name) != nil {
		return false
	}
	switch strings.ToLower(name) {
	case "iridium", "iridium_imt", "iridium_sbd", "satellite", "globalstar":
		return true
	}
	return false
}

func bucket(at time.Time) int64 { return at.Unix() / 600 }

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func normalise(text string) string { return strings.ToLower(strings.Join(strings.Fields(text), " ")) }

func replyKey(tenantID, imei, body string, at time.Time) string {
	return fmt.Sprintf("satchat:mt:%s:%s:%d", tenantID, digest(imei, normalise(body)), bucket(at))
}

func echoKey(tenantID, imei, text string, at time.Time) string {
	return fmt.Sprintf("satchat:echo:%s:%s:%d", tenantID, digest(imei, normalise(text)), bucket(at))
}

func suffix(id string) string {
	if len(id) <= 4 {
		return id
	}
	return "…" + id[len(id)-4:]
}
