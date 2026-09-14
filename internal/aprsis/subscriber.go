package aprsis

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// Subscriber listens on Hub MQTT position topics and injects satellite-originated
// positions into APRS-IS. Also receives APRS-IS messages addressed to MeshSat
// devices and forwards them to MQTT.
type Subscriber struct {
	mqtt    bus.MessageBus
	pool    *ConnPool      // one connection per tenant that brought a callsign
	tenants TenantResolver // whose traffic is this?

	// Rate limiting: max 1 position per device per coalesceSec.
	// Keyed by tenant AND device: two tenants' devices are different radios on
	// different licences, and one must not consume the other's budget.
	coalesceSec int
	lastSent    map[string]time.Time
	mu          sync.Mutex
	// claimer makes exactly one replica transmit a given message.
	claimer Claimer
}

// Claimer records a key once across every replica (store.ClaimOnce).
//
// Declared here as a one-method interface so this package does not import the
// store: it is an MQTT-to-APRS bridge and has no other business with it.
type Claimer interface {
	ClaimOnce(ctx context.Context, key string) (bool, error)
}

// SetClaimer makes exactly one replica transmit each message (MESHSAT-1120).
//
// Both replicas subscribe to every topic with a plain Subscribe, and shouldSend
// is a per-REPLICA map, so without this both would inject the same packet into
// APRS-IS -- a public network, under our own callsign.
func (s *Subscriber) SetClaimer(c Claimer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimer = c
}

// claim reports whether THIS replica should transmit.
//
// It fails CLOSED, which is the opposite of webhook delivery, and deliberately.
// A webhook is our own customer's endpoint and a lost event matters more than a
// duplicate. APRS-IS is somebody else's shared infrastructure: a duplicate
// packet under our callsign is antisocial and is the kind of thing that gets a
// callsign filtered, while a missed position is invisible on a best-effort
// feed nobody's safety depends on.
func (s *Subscriber) claim(topic string, payload []byte) bool {
	s.mu.Lock()
	c := s.claimer
	s.mu.Unlock()
	if c == nil {
		return true // single-replica or unconfigured: behave as before
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	won, err := c.ClaimOnce(ctx, "aprs:"+hubmqtt.MessageDigest(topic, payload))
	if err != nil {
		slog.Warn("aprsis: claim failed, not transmitting", "error", err)
		return false
	}
	return won
}

// NewSubscriber creates a new APRS-IS MQTT subscriber. Each message is injected
// under the callsign of the tenant that owns the device, or not at all.
func NewSubscriber(mqtt bus.MessageBus, pool *ConnPool, tenants TenantResolver, coalesceSec int) *Subscriber {
	if coalesceSec <= 0 {
		coalesceSec = 60
	}
	return &Subscriber{
		mqtt:        mqtt,
		pool:        pool,
		tenants:     tenants,
		coalesceSec: coalesceSec,
		lastSent:    make(map[string]time.Time),
	}
}

// Start subscribes to position MQTT topics and sets up the APRS-IS inbound
// handler. Every subscription goes through routeByTenant. [MESHSAT-1032/-1121]
func (s *Subscriber) Start() error {
	if s.tenants == nil {
		return fmt.Errorf("aprsis subscriber: no tenant resolver; refusing to inject any tenant's positions")
	}
	for _, f := range hubmqtt.DualFilters("meshsat/+/position") {
		if err := s.mqtt.Subscribe(f, 1, routeByTenant(s.tenants, s.handlePosition)); err != nil {
			return err
		}
	}
	if err := s.subscribeMO(); err != nil {
		return fmt.Errorf("aprsis subscriber: %w", err)
	}

	// Inbound: APRS-IS messages -> MQTT, per connection so a message addressed to
	// one tenant's callsign cannot surface in another tenant's topic space.
	if s.pool != nil {
		s.pool.SetPacketHandler(s.handleInboundPacket)
	}

	slog.Info("aprsis: subscriber started", "coalesce_sec", s.coalesceSec)
	return nil
}

// positionMsg matches the JSON on meshsat/{device_id}/position.
type positionMsg struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Source    string  `json:"source,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
}

func (s *Subscriber) handlePosition(tenantID, topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	// Whose licence is this going out under? nil means the tenant has no APRS
	// account, has paused transmission, or its connection is not up -- all of
	// which mean "do not transmit", never "use somebody else's callsign".
	client := s.pool.ForTenant(tenantID)
	if client == nil {
		return
	}

	var pos positionMsg
	if err := json.Unmarshal(payload, &pos); err != nil {
		return
	}

	// Only inject satellite-originated positions (not mesh/SMS)
	if pos.Source != "iridium" && pos.Source != "iridium_cep" && pos.Source != "globalstar" {
		return
	}

	if pos.Lat == 0 && pos.Lon == 0 {
		return
	}

	// Rate limit per tenant+device
	if !s.shouldSend(tenantID, deviceID) {
		return
	}

	// One replica transmits, not both.
	if !s.claim(topic, payload) {
		return
	}

	comment := fmt.Sprintf("MeshSat via %s", pos.Source)
	packet := FormatPosition(client.callsign, client.ssid, pos.Lat, pos.Lon, comment)

	if err := client.Send(packet); err != nil {
		slog.Warn("aprsis: send position failed", "error", err, "tenant", tenantID, "device", deviceID)
		return
	}
	slog.Debug("aprsis: position injected", "tenant", tenantID, "callsign", client.FormatCallsign(),
		"device", deviceID, "lat", pos.Lat, "lon", pos.Lon)
}

// moDecodedMsg matches meshsat/{device_id}/mo/decoded.
type moDecodedMsg struct {
	IMEI       string  `json:"imei"`
	Text       string  `json:"text"`
	IridiumLat float64 `json:"iridium_latitude,omitempty"`
	IridiumLon float64 `json:"iridium_longitude,omitempty"`
}

func (s *Subscriber) handleMODecoded(tenantID, topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	client := s.pool.ForTenant(tenantID)
	if client == nil {
		return
	}

	var mo moDecodedMsg
	if err := json.Unmarshal(payload, &mo); err != nil {
		return
	}

	// Only inject if we have Iridium coordinates
	if mo.IridiumLat == 0 && mo.IridiumLon == 0 {
		return
	}

	if !s.shouldSend(tenantID, deviceID) {
		return
	}

	if !s.claim(topic, payload) {
		return
	}

	comment := "MeshSat via Iridium SBD"
	if mo.Text != "" {
		// Truncate text for APRS comment (max ~40 chars practical)
		text := mo.Text
		if len(text) > 40 {
			text = text[:37] + "..."
		}
		comment += " " + text
	}

	packet := FormatPosition(client.callsign, client.ssid, mo.IridiumLat, mo.IridiumLon, comment)

	if err := client.Send(packet); err != nil {
		slog.Warn("aprsis: send MO position failed", "error", err, "tenant", tenantID, "device", deviceID)
		return
	}
	slog.Debug("aprsis: MO position injected", "tenant", tenantID, "device", deviceID)
}

// handleInboundPacket processes APRS-IS packets addressed to a tenant's callsign.
//
// tenantID and callsign come from the CONNECTION the packet arrived on, not from
// the packet: each tenant has its own socket, logged in under its own licence, so
// the addressee can only be that tenant's callsign (MESHSAT-1121).
func (s *Subscriber) handleInboundPacket(tenantID, callsign, line string) {
	// APRS message format: SRC>DST,PATH::ADDRESSEE :message{id
	// We're looking for messages addressed to this connection's callsign
	myCall := strings.ToUpper(callsign)

	colonIdx := strings.Index(line, ":")
	if colonIdx < 0 || colonIdx+1 >= len(line) {
		return
	}

	info := line[colonIdx+1:]
	if len(info) < 2 || info[0] != ':' {
		return // not a message packet
	}

	// Extract addressee (9 chars padded with spaces)
	if len(info) < 11 {
		return
	}
	addressee := strings.TrimSpace(info[1:10])

	if !strings.HasPrefix(strings.ToUpper(addressee), myCall) {
		return // not for us
	}

	// Extract message text
	if len(info) < 12 || info[10] != ':' {
		return
	}
	msgText := info[11:]

	// Strip message ID if present
	if idx := strings.LastIndex(msgText, "{"); idx >= 0 {
		msgText = msgText[:idx]
	}

	// Extract source callsign
	srcEnd := strings.Index(line, ">")
	if srcEnd < 0 {
		return
	}
	srcCall := line[:srcEnd]

	slog.Info("aprsis: inbound message", "tenant", tenantID, "from", srcCall, "to", addressee, "text", msgText)

	// Publish to MQTT — use the addressee as a device hint
	msg := map[string]interface{}{
		"source":   "aprs-is",
		"from":     srcCall,
		"to":       addressee,
		"text":     msgText,
		"received": time.Now().UTC().Format(time.RFC3339),
	}

	// The tenant's own topic space. Publishing every tenant's inbound traffic to
	// the one legacy hub topic would hand each of them the others' messages.
	topic := hubmqtt.TopicAPRSInboundFor(tenantID)
	if err := s.mqtt.PublishJSON(topic, 1, false, msg); err != nil {
		slog.Warn("aprsis: publish inbound failed", "tenant", tenantID, "error", err)
	}
}

// shouldSend returns true if enough time has passed since the last send for this
// tenant's device. The key carries the tenant because two tenants may legitimately
// register the same device id, and one tenant must never be able to silence
// another's beacons by consuming the coalesce window.
func (s *Subscriber) shouldSend(tenantID, deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := tenantID + "\x00" + deviceID
	now := time.Now()
	if last, ok := s.lastSent[key]; ok {
		if now.Sub(last) < time.Duration(s.coalesceSec)*time.Second {
			return false
		}
	}
	s.lastSent[key] = now
	return true
}

func (s *Subscriber) subscribeMO() error {
	for _, f := range hubmqtt.DualFilters("meshsat/+/mo/decoded") {
		if err := s.mqtt.Subscribe(f, 1, routeByTenant(s.tenants, s.handleMODecoded)); err != nil {
			return err
		}
	}
	return nil
}
