// Package isolation drives the Hub's real consumers end to end against a real
// store, the way two replicas run them, and asks one question: can a message
// that one tenant is able to publish end up as another tenant's?
//
// It has no build tag on purpose. The CI test job runs `go test ./...` with no
// tags, so a test under test/e2e or test/integration would not guard anything.
//
// The victim is store.DefaultTenantID wherever there is one: these bugs have no
// tenant dimension, so a regression collapses onto the default tenant and a test
// between two customer tenants passes by accident (.claude/rules/tenancy.md).
package isolation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/meshsat/meshsat-hub/internal/api"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/globalstar"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/mesh"
	"github.com/meshsat/meshsat-hub/internal/message"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/position"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/routing"
	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/sos"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

const (
	victim   = store.DefaultTenantID
	attacker = "t_attacker"
	bystand  = "t_bystander"

	victimIMEI   = "300234063904190"
	customerIMEI = "300234063904191"
	attackerIMEI = "300234063904192"
	victimBridge = "victim-kit"

	phoneNumber = "+31612345678"
	smsSecret   = "platform-relay-secret"
	smsSecretB  = "bystander-relay-secret"
)

type published struct {
	topic   string
	payload []byte
}

// memBus is an in-process broker: MQTT wildcards, every subscriber of a matching
// filter gets every message (the Hub's replicas use plain Subscribe, not queue
// groups), delivered synchronously so a test has no timing in it.
type memBus struct {
	mu   sync.Mutex
	subs []memSub
	log  []published
}

type memSub struct {
	filter  string
	handler bus.MessageHandler
}

func (b *memBus) Connect() error    { return nil }
func (b *memBus) Disconnect()       {}
func (b *memBus) IsConnected() bool { return true }

func (b *memBus) Subscribe(filter string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, memSub{filter, h})
	return nil
}

func (b *memBus) QueueSubscribe(filter string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(filter, q, h)
}

func (b *memBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	b.log = append(b.log, published{topic, payload})
	var matched []bus.MessageHandler
	for _, s := range b.subs {
		if filterMatches(s.filter, topic) {
			matched = append(matched, s.handler)
		}
	}
	b.mu.Unlock()
	for _, h := range matched {
		h(topic, payload)
	}
	return nil
}

func (b *memBus) PublishJSON(topic string, q byte, r bool, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Publish(topic, q, r, data)
}

func (b *memBus) count(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, p := range b.log {
		if p.topic == topic {
			n++
		}
	}
	return n
}

func filterMatches(filter, topic string) bool {
	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(t) || (seg != "+" && seg != t[i]) {
			return false
		}
	}
	return len(f) == len(t)
}

// world is one Hub: one store and one broker, two replicas of every consumer.
type world struct {
	t     *testing.T
	store store.Store
	bus   *memBus
	r     *tenancy.Resolver

	mu     sync.Mutex
	routed map[string]int // tenant + "|" + device -> dispatches
}

func newWorld(t *testing.T) *world {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, id := range []string{attacker, bystand} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Name: id, Status: "active"}); err != nil {
			t.Fatalf("tenant %s: %v", id, err)
		}
	}
	w := &world{t: t, store: s, bus: &memBus{}, routed: map[string]int{}}

	for _, d := range []struct{ tenant, imei string }{
		{victim, victimIMEI}, {bystand, customerIMEI}, {attacker, attackerIMEI},
	} {
		if err := s.CreateDevice(ctx, d.tenant, &store.Device{IMEI: d.imei, Label: d.imei, Type: "rockblock"}); err != nil {
			t.Fatalf("device %s: %v", d.imei, err)
		}
	}
	if err := s.CreateOrUpdateBridge(ctx, victim, &store.Bridge{BridgeID: victimBridge, Label: "victim"}); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	for _, tenant := range []string{victim, attacker, bystand} {
		if err := s.CreateRoute(ctx, tenant, &store.Route{
			ID: "capture-" + tenant, Name: "capture", SourceType: "*", DestinationType: "capture", Enabled: true,
		}); err != nil {
			t.Fatalf("route: %v", err)
		}
	}

	for range 2 {
		w.startReplica()
	}
	return w
}

// startReplica wires one replica's consumers with their own resolver cache, as
// cmd/meshsat-hub does. The last resolver stays on w for the provider handlers.
func (w *world) startReplica() {
	w.t.Helper()
	r := tenancy.NewResolver(w.store, store.DefaultTenantID, 30*time.Second)
	w.r = r

	engine := routing.NewEngine(w.store, w.bus, r)
	engine.RegisterHandler("capture", func(ctx context.Context, route *store.Route, deviceID string, _ json.RawMessage) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.routed[tenancy.FromContext(ctx)+"|"+deviceID]++
	})
	inbound := sms.NewInboundSubscriber(w.bus, w.store, store.DefaultTenantID)
	inbound.SetTenants(r)
	starters := []interface{ Start() error }{
		inbound,
		message.NewSubscriber(w.bus, w.store, r),
		position.NewSubscriber(w.bus, w.store, r),
		sos.NewDetector(w.bus, nil, w.store, r, ""),
		mesh.NewRecorder(w.bus, w.store, r),
		engine,
	}
	for _, s := range starters {
		if err := s.Start(); err != nil {
			w.t.Fatalf("start: %v", err)
		}
	}
}

func (w *world) routedTo(tenant, device string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.routed[tenant+"|"+device]
}

func (w *world) messages(tenant, device string) []store.Message {
	w.t.Helper()
	msgs, err := w.store.ListMessages(context.Background(), tenant, device, 50)
	if err != nil {
		w.t.Fatalf("list messages: %v", err)
	}
	return msgs
}

func (w *world) positions(tenant, device string) int {
	w.t.Helper()
	ps, err := w.store.ListPositions(context.Background(), tenant, device, 50)
	if err != nil {
		w.t.Fatalf("list positions: %v", err)
	}
	return len(ps)
}

func (w *world) publishJSON(topic string, v any) {
	w.t.Helper()
	if err := w.bus.PublishJSON(topic, 1, false, v); err != nil {
		w.t.Fatalf("publish: %v", err)
	}
}

func decoded(id, imei, text string) map[string]any {
	return map[string]any{"id": id, "imei": imei, "text": text, "channel": "iridium"}
}

func fix(lat float64) map[string]any {
	return map[string]any{"lat": lat, "lon": 4.3, "source": "gps", "timestamp": time.Now().UTC().Format(time.RFC3339)}
}

// refusedTotal reads the refusal counter out of the default registry by name.
func refusedTotal(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != "meshsat_hub_tenancy_topic_refused_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// cloudloopMO is the delivery a real Cloudloop webhook makes for this modem.
func cloudloopMO(t *testing.T, imei, text string) []byte {
	t.Helper()
	mo := cloudloop.LingoMO{
		ID:         "lingo-" + imei,
		ReceivedAt: cloudloop.LingoTimestamp{Year: 2026, Month: 11, Day: 8, Hour: 12},
		Identity: cloudloop.LingoIdentity{
			AccountID: "acct",
			Hardware:  &cloudloop.LingoHardware{ID: "hw", Type: "ROCKBLOCK_9704", IMEI: imei, Serial: "rb"},
			ThingID:   "thing",
		},
		IMT:     &cloudloop.LingoIMT{CMID: "cm", Topic: "IMT_TOPIC_PURPLE", MessageID: json.Number("1"), Size: len(text)},
		Message: base64.StdEncoding.EncodeToString([]byte(text)),
	}
	body, err := json.Marshal(mo)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// deliverCloudloop posts to the real handler on the platform account's path,
// which carries no tenant: the owner has to be read from the IMEI.
func (w *world) deliverCloudloop(imei, text string) {
	w.t.Helper()
	h := cloudloop.NewWebhookHandler(w.bus)
	h.SetAllowedIPs([]string{"192.0.2.1"}) // httptest's RemoteAddr
	h.SetTenants(w.r)
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(cloudloopMO(w.t, imei, text)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("cloudloop delivery: %d %s", rec.Code, rec.Body.String())
	}
}

// realMessageID is the id the real pipeline gives this modem's next MO, read off
// a throwaway Hub. It is what an attacker would predict and forge.
func realMessageID(t *testing.T, imei, text string) string {
	t.Helper()
	w := newWorld(t)
	w.deliverCloudloop(imei, text)
	w.bus.mu.Lock()
	defer w.bus.mu.Unlock()
	for _, p := range w.bus.log {
		if p.topic == hubmqtt.TopicMODecodedFor(victim, imei) {
			var m struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(p.payload, &m); err == nil && m.ID != "" {
				return m.ID
			}
		}
	}
	t.Fatal("the real pipeline published no decoded MO to learn the id from")
	return ""
}

// MESHSAT-1469. Every bridge may publish mo/decoded, position and sos for any
// device id in its own namespace (bridge.NATSPermissions), so a customer can put
// the platform's IMEI in its own topic. None of that may become the platform's.
func TestAForgedMessageNeverBecomesTheOwnersOnEitherReplica(t *testing.T) {
	const sosText = "SOS need help"
	forgedID := realMessageID(t, victimIMEI, sosText)

	w := newWorld(t)
	before := refusedTotal(t)

	// In the topic, and with the id of the victim's next real message.
	w.publishJSON(hubmqtt.TopicMODecodedFor(attacker, victimIMEI), decoded(forgedID, victimIMEI, sosText))
	w.publishJSON(hubmqtt.TopicPositionFor(attacker, victimIMEI), fix(52.1))
	// Only in the payload: the attacker's own device on its own topic, naming
	// the victim's modem in the imei field the consumers read.
	w.publishJSON(hubmqtt.TopicMODecodedFor(attacker, attackerIMEI), decoded("forged-2", victimIMEI, sosText))

	if n := len(w.messages(victim, victimIMEI)); n != 0 {
		t.Errorf("the victim's history has %d forged messages", n)
	}
	if n := w.positions(victim, victimIMEI); n != 0 {
		t.Errorf("the victim's track has %d forged positions", n)
	}
	if n := w.routedTo(victim, victimIMEI); n != 0 {
		t.Errorf("the victim's routes fired %d times on a forgery", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(victim, victimIMEI)); n != 0 {
		t.Errorf("%d SOS events were raised on the victim's chain by a forgery", n)
	}
	if n := len(w.messages(attacker, victimIMEI)); n != 0 {
		t.Errorf("a forgery was filed in the attacker's own tenant under the victim's IMEI: %d", n)
	}
	if refusedTotal(t) <= before {
		t.Error("the refusals were not counted")
	}

	// And the forgery claimed nothing: the REAL message with that very id goes
	// through the provider path and reaches every consumer exactly once.
	w.deliverCloudloop(victimIMEI, sosText)

	if n := len(w.messages(victim, victimIMEI)); n != 1 {
		t.Errorf("the real message was stored %d times, want 1", n)
	}
	if n := w.routedTo(victim, victimIMEI); n != 1 {
		t.Errorf("the real message fired the victim's route %d times, want 1", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(victim, victimIMEI)); n != 1 {
		t.Errorf("the real SOS raised %d events, want 1", n)
	}
}

// The phone gateway's SMS topic is the same grant and the same forgery.
func TestAForgedInboundSMSNeverBecomesTheOwners(t *testing.T) {
	w := newWorld(t)
	const sender = "+31600000001"

	w.publishJSON(hubmqtt.DeviceTopic(attacker, victimIMEI, "sms/inbound"), map[string]any{"from": sender, "body": "forged"})
	if n := len(w.messages(victim, sender)); n != 0 {
		t.Errorf("the victim's history has %d forged SMS", n)
	}

	w.publishJSON(hubmqtt.DeviceTopic(victim, victimIMEI, "sms/inbound"), map[string]any{"from": sender, "body": "real"})
	if n := len(w.messages(victim, sender)); n != 1 {
		t.Errorf("the owner's own SMS was stored %d times, want 1", n)
	}
}

// moveModem seeds the resolver with the old owner, then moves the row, which is
// the state a replica is in during the cache's TTL after a modem changes hands.
func (w *world) moveModem(imei, from, to string) {
	w.t.Helper()
	ctx := context.Background()
	if got := w.r.ForDevice(ctx, imei); got != from {
		w.t.Fatalf("seeding: owner %q, want %q", got, from)
	}
	if err := w.store.DeleteDevice(ctx, from, imei); err != nil {
		w.t.Fatalf("delete: %v", err)
	}
	if err := w.store.CreateDevice(ctx, to, &store.Device{IMEI: imei, Label: imei, Type: "rockblock"}); err != nil {
		w.t.Fatalf("create: %v", err)
	}
	if got := w.r.ForDevice(ctx, imei); got != from {
		w.t.Fatalf("the cache should still say %q, says %q", from, got)
	}
}

// The consumers refuse a namespace that is not the owner's, so a provider
// publishing on a stale cached owner would lose a real message.
func TestAProviderPublishesUnderTheOwnerOfThisMoment(t *testing.T) {
	t.Run("cloudloop", func(t *testing.T) {
		w := newWorld(t)
		w.moveModem(victimIMEI, victim, bystand)
		w.deliverCloudloop(victimIMEI, "hello")
		if n := w.bus.count(hubmqtt.TopicMODecodedFor(bystand, victimIMEI)); n != 1 {
			t.Errorf("published %d times under the new owner, want 1", n)
		}
		if n := w.bus.count(hubmqtt.TopicMODecodedFor(victim, victimIMEI)); n != 0 {
			t.Errorf("published %d times under the old owner", n)
		}
	})
	t.Run("globalstar", func(t *testing.T) {
		w := newWorld(t)
		w.moveModem(victimIMEI, victim, bystand)
		const secret = "gs-secret"
		h := globalstar.NewHandler(w.bus, secret)
		h.SetTenants(w.r)
		body, _ := json.Marshal(globalstar.MOPayload{
			DeviceID: victimIMEI, MessageID: "gs-1", ReceivedAt: "2026-11-08T12:00:00Z",
			Data: base64.StdEncoding.EncodeToString([]byte("hello")),
		})
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/globalstar", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
		}
		if n := w.bus.count(hubmqtt.TopicMODecodedFor(bystand, victimIMEI)); n != 1 {
			t.Errorf("published %d times under the new owner, want 1", n)
		}
		if n := w.bus.count(hubmqtt.TopicMODecodedFor(victim, victimIMEI)); n != 0 {
			t.Errorf("published %d times under the old owner", n)
		}
	})
}

// The provider paths have no tenant context of their own, which is the reason
// the owner lookup exists: a customer's modem on the platform account's path.
func TestASatelliteMessageStillReachesItsRegisteredOwner(t *testing.T) {
	w := newWorld(t)
	before := refusedTotal(t)

	w.deliverCloudloop(customerIMEI, "SOS stuck on the ridge")

	if n := len(w.messages(bystand, customerIMEI)); n != 1 {
		t.Errorf("owner's history has %d messages, want 1", n)
	}
	if n := w.routedTo(bystand, customerIMEI); n != 1 {
		t.Errorf("owner's route fired %d times, want 1", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(bystand, customerIMEI)); n != 1 {
		t.Errorf("owner's SOS raised %d events, want 1", n)
	}
	if n := len(w.messages(victim, customerIMEI)); n != 0 {
		t.Errorf("the platform kept %d of a customer's messages", n)
	}
	if got := refusedTotal(t); got != before {
		t.Errorf("a real satellite message was refused: counter moved by %v", got-before)
	}
}

// Traffic in its owner's own namespace is the normal case and must not be
// touched; this is also what keeps the refusal assertions above from passing
// because nothing is ever stored.
func TestAMessageInItsOwnersNamespaceIsFiled(t *testing.T) {
	w := newWorld(t)
	before := refusedTotal(t)

	w.publishJSON(hubmqtt.TopicMODecodedFor(attacker, attackerIMEI), decoded("own-1", attackerIMEI, "hello"))
	w.publishJSON(hubmqtt.TopicPositionFor(attacker, attackerIMEI), fix(52.2))
	w.publishJSON(hubmqtt.TopicMODecodedFor(victim, victimIMEI), decoded("own-2", victimIMEI, "hello"))
	w.publishJSON(hubmqtt.TopicPositionFor(victim, victimIMEI), fix(52.3))

	if n := len(w.messages(attacker, attackerIMEI)); n != 1 {
		t.Errorf("tenant's own message stored %d times, want 1", n)
	}
	if n := w.positions(attacker, attackerIMEI); n != 1 {
		t.Errorf("tenant's own position stored %d times, want 1", n)
	}
	if n := len(w.messages(victim, victimIMEI)); n != 1 {
		t.Errorf("platform's own message stored %d times, want 1", n)
	}
	if n := w.positions(victim, victimIMEI); n != 1 {
		t.Errorf("platform's own position stored %d times, want 1", n)
	}
	if got := refusedTotal(t); got != before {
		t.Errorf("legitimate traffic was refused: counter moved by %v", got-before)
	}
}

// Only the Hub and the operator's own bridges can write the platform namespace
// (bridge.NATSPermissions), and a provider handler that could not read the owner
// publishes there. Refusing it would lose a real SOS during a database blip, so
// it keeps resolving to the owner of record.
func TestThePlatformNamespaceKeepsTheOwnerOfRecord(t *testing.T) {
	w := newWorld(t)
	before := refusedTotal(t)

	w.publishJSON(hubmqtt.TopicMODecodedFor(victim, customerIMEI), decoded("legacy-1", customerIMEI, "SOS"))

	if n := len(w.messages(bystand, customerIMEI)); n != 1 {
		t.Errorf("owner has %d messages, want 1", n)
	}
	if n := len(w.messages(victim, customerIMEI)); n != 0 {
		t.Errorf("the platform kept %d of a customer's messages", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(bystand, customerIMEI)); n != 1 {
		t.Errorf("owner's SOS raised %d events, want 1", n)
	}
	if got := refusedTotal(t); got != before {
		t.Errorf("platform-namespace traffic was refused: counter moved by %v", got-before)
	}
}

// A device nobody registered joins the tenant whose namespace it spoke in.
func TestAnUnregisteredDeviceStaysInItsTopicsTenant(t *testing.T) {
	w := newWorld(t)
	const fresh = "300234063904199"

	w.publishJSON(hubmqtt.TopicMODecodedFor(attacker, fresh), decoded("fresh-1", fresh, "hello"))

	if n := len(w.messages(attacker, fresh)); n != 1 {
		t.Errorf("tenant has %d messages from its unregistered device, want 1", n)
	}
	if n := len(w.messages(victim, fresh)); n != 0 {
		t.Errorf("the platform kept %d messages from another tenant's namespace", n)
	}
}

// mo/decoded names a bridge in its payload, so presence is the same forgery
// with a bridge id instead of an IMEI.
func TestAForgedBridgeIDRecordsNothingForItsOwner(t *testing.T) {
	w := newWorld(t)
	since := time.Now().Add(-time.Hour)
	forged := map[string]any{"bridge_id": victimBridge, "device_id": "!a1b2c3d4", "channel": "mesh", "text": "x"}

	w.publishJSON(hubmqtt.TopicMODecodedFor(attacker, "!a1b2c3d4"), forged)

	nodes, err := w.store.MeshNodesSeenSince(context.Background(), victim, victimBridge, since)
	if err != nil {
		t.Fatalf("mesh nodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("a forged bridge id recorded %d nodes behind the victim's kit", len(nodes))
	}

	w.publishJSON(hubmqtt.TopicMODecodedFor(victim, "!a1b2c3d4"), forged)
	nodes, err = w.store.MeshNodesSeenSince(context.Background(), victim, victimBridge, since)
	if err != nil {
		t.Fatalf("mesh nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("the kit's own traffic recorded %d nodes, want 1", len(nodes))
	}
}

// APRS-IS transmits under the tenant's amateur licence, so the same forgery
// would key up a transmission on somebody else's callsign.
func TestAForgedTopicCannotBorrowTheOwnersCallsign(t *testing.T) {
	w := newWorld(t)
	if got := w.r.TenantForTopic(context.Background(), hubmqtt.TopicPositionFor(attacker, victimIMEI)); got != "" {
		t.Errorf("TenantForTopic answered %q for a forged topic, want none", got)
	}
	if got := w.r.TenantForTopic(context.Background(), hubmqtt.TopicPositionFor(victim, victimIMEI)); got != victim {
		t.Errorf("TenantForTopic answered %q for the owner's own topic", got)
	}
}

// devicesRouter is POST /api/devices as a signed-in member of the tenant.
func devicesRouter(s store.Store, tenant string) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), hubauth.TenantContextKey, tenant)))
		})
	})
	r.Post("/api/devices", api.NewDeviceHandler(s).CreateDevice)
	return r
}

func register(t *testing.T, s store.Store, tenant, imei string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"imei": imei, "label": "x", "type": "rockblock"})
	req := httptest.NewRequest(http.MethodPost, "/api/devices", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	devicesRouter(s, tenant).ServeHTTP(rec, req)
	return rec.Code
}

// MESHSAT-1688. A device id is a modem's.
func TestADeviceIDIsAModemsAtRegistration(t *testing.T) {
	w := newWorld(t)
	refused := []string{
		phoneNumber, "+", "+300234063904193", "31612345678", "12345", "MeshSat",
		"3002340639041931", "30023406390419", "30023406390419a", " 300234063904193",
		"300234063904193\n", "0-123456", "0-12345678", "dev-gs-001", "E2E1762600000",
	}
	for _, id := range refused {
		if code := register(t, w.store, attacker, id); code != http.StatusBadRequest {
			t.Errorf("registering %q: %d, want 400", id, code)
		}
		if owner, err := w.store.LookupDeviceTenant(context.Background(), id); err == nil {
			t.Errorf("%q was registered to %s", id, owner)
		}
	}
	accepted := []string{"300234063904193", "0-1234567", "1234567", "01234567"}
	for _, id := range accepted {
		if code := register(t, w.store, attacker, id); code != http.StatusCreated {
			t.Errorf("registering modem id %q: %d, want 201", id, code)
		}
	}
}

// A device birth is a registration too: it was the second way in.
func TestABirthCannotRegisterAPhoneNumber(t *testing.T) {
	w := newWorld(t)
	sub := bridge.NewSubscriber(w.bus, w.store, w.r)
	if err := sub.Start(); err != nil {
		t.Fatalf("subscriber: %v", err)
	}
	birth := func(imei string) {
		w.publishJSON("meshsat/"+attacker+"/bridge/kit1/device/iridium_0/birth", protocol.DeviceBirth{
			Protocol: protocol.ProtocolVersion, DeviceID: "iridium_0", BridgeID: "kit1",
			Type: "iridium_sbd", IMEI: imei, Timestamp: time.Now(),
		})
	}

	birth(phoneNumber)
	if owner, err := w.store.LookupDeviceTenant(context.Background(), phoneNumber); err == nil {
		t.Errorf("a birth registered a phone number to %s", owner)
	}

	const modem = "300234063904194"
	birth(modem)
	if owner, err := w.store.LookupDeviceTenant(context.Background(), modem); err != nil || owner != attacker {
		t.Errorf("a real modem's birth: owner %q, err %v", owner, err)
	}
}

func (w *world) smsHandler() *sms.WebhookHandler {
	h := sms.NewWebhookHandler(w.bus, smsSecret)
	h.SetTenants(w.r)
	h.SetStore(w.store)
	return h
}

func smsRequest(secret, sid, body string) *http.Request {
	form := url.Values{"From": {phoneNumber}, "To": {"+3197010258258"}, "Body": {body}, "MessageSid": {sid}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(phoneNumber + body))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

// plant writes the row the way an older Hub could have, with no format check.
func (w *world) plant(tenant, id string) {
	w.t.Helper()
	if err := w.store.CreateDevice(context.Background(), tenant, &store.Device{IMEI: id, Label: "planted", Type: "rockblock"}); err != nil {
		w.t.Fatalf("plant: %v", err)
	}
}

// MESHSAT-1688: a phone number somebody registered does not pull that number's
// SMS out of the tenant whose path it arrived at. The victim is the platform's
// own number, and the row is planted, because rows from before the format check
// may exist on a self-hosted Hub.
func TestAnSMSOnThePlatformsPathStaysThePlatforms(t *testing.T) {
	w := newWorld(t)
	w.plant(attacker, phoneNumber)
	before := refusedTotal(t)

	rec := httptest.NewRecorder()
	w.smsHandler().ServeHTTP(rec, smsRequest(smsSecret, "SM-platform", "MAYDAY lost at sea"))
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}

	if n := len(w.messages(victim, phoneNumber)); n != 1 {
		t.Errorf("the platform has %d of its SMS, want 1", n)
	}
	if n := len(w.messages(attacker, phoneNumber)); n != 0 {
		t.Errorf("the tenant that registered the number captured %d SMS", n)
	}
	if n := w.routedTo(attacker, phoneNumber); n != 0 {
		t.Errorf("the captor's route fired %d times", n)
	}
	if n := w.routedTo(victim, phoneNumber); n != 1 {
		t.Errorf("the platform's route fired %d times, want 1", n)
	}
	if n := w.bus.count(hubmqtt.TopicMODecodedFor(attacker, phoneNumber)); n != 0 {
		t.Errorf("published %d times in the captor's namespace", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(victim, phoneNumber)); n != 1 {
		t.Errorf("an SMS MAYDAY raised %d events on the platform, want 1", n)
	}
	if n := w.bus.count(hubmqtt.TopicSOSFor(attacker, phoneNumber)); n != 0 {
		t.Errorf("an SMS MAYDAY raised %d events on the captor", n)
	}
	if got := refusedTotal(t); got != before {
		t.Errorf("an SMS was refused for a number somebody registered: counter moved by %v", got-before)
	}
}

func TestAnSMSOnATenantsPathStaysThatTenants(t *testing.T) {
	w := newWorld(t)
	w.plant(attacker, phoneNumber)

	req := smsRequest(smsSecretB, "SM-tenant", "hello there")
	req = req.WithContext(webhookroute.NewContext(req.Context(), &webhookroute.Resolved{
		TenantID: bystand,
		Account:  &integrations.Account{TenantID: bystand, Fields: map[string]string{"webhook_secret": smsSecretB}},
	}))
	rec := httptest.NewRecorder()
	w.smsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}

	if n := len(w.messages(bystand, phoneNumber)); n != 1 {
		t.Errorf("the path's tenant has %d of its SMS, want 1", n)
	}
	if n := len(w.messages(attacker, phoneNumber)); n != 0 {
		t.Errorf("the tenant that registered the number captured %d SMS", n)
	}
	if n := len(w.messages(victim, phoneNumber)); n != 0 {
		t.Errorf("the platform kept %d of a tenant's SMS", n)
	}
	if n := w.bus.count(hubmqtt.TopicMODecodedFor(bystand, phoneNumber)); n != 1 {
		t.Errorf("published %d times in the path tenant's namespace, want 1", n)
	}
}
