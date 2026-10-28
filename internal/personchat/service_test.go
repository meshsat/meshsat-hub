package personchat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/email"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ---- fakes ----

type fakeStore struct {
	mu       sync.Mutex
	claimErr error
	peerErr  error
	buckets  map[string]int
	claims   map[string]bool
	tenants  map[string]*store.Tenant
	devices  map[string]string // imei -> owner
	labels   map[string]string
	profiles map[string]string // imei -> profile ("" or "post")
	bridges  map[string][]*store.Bridge
	peers    map[string]*store.OOBPeer // tenant|bridge
	convs    map[string]*store.PersonConversation
	sends    []store.PersonChatSend
}

func newFakeStore() *fakeStore {
	return &fakeStore{buckets: map[string]int{}, claims: map[string]bool{}, tenants: map[string]*store.Tenant{}, devices: map[string]string{},
		labels: map[string]string{}, profiles: map[string]string{}, bridges: map[string][]*store.Bridge{}, peers: map[string]*store.OOBPeer{},
		convs: map[string]*store.PersonConversation{}}
}

func (f *fakeStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}
func (f *fakeStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tenants[id]; ok {
		c := *t
		return &c, nil
	}
	return &store.Tenant{ID: id}, nil
}
func (f *fakeStore) GetDevice(_ context.Context, tenantID, imei string) (*store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices[imei] != tenantID {
		return nil, nil
	}
	return &store.Device{IMEI: imei, Label: f.labels[imei], Profile: f.profiles[imei]}, nil
}
func (f *fakeStore) ListBridges(_ context.Context, tenantID string) ([]*store.Bridge, error) {
	return f.bridges[tenantID], nil
}

// GetOOBPeer answers like the real stores: sql.ErrNoRows for a kit with no
// pairing, which is most of them.
func (f *fakeStore) GetOOBPeer(_ context.Context, tenantID, bridgeID string) (*store.OOBPeer, error) {
	if f.peerErr != nil {
		return nil, f.peerErr
	}
	if p, ok := f.peers[tenantID+"|"+bridgeID]; ok {
		return p, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) TakePersonChatSlot(_ context.Context, tenantID, key string, max int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := tenantID + "|" + key
	if f.buckets[k] >= max {
		return false, nil
	}
	f.buckets[k]++
	return true, nil
}
func (f *fakeStore) SavePersonConversationOut(_ context.Context, c *store.PersonConversation) (*store.PersonConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := c.TenantID + "|" + c.ID
	if old, ok := f.convs[k]; ok {
		cp := *old
		return &cp, nil
	}
	cp := *c
	f.convs[k] = &cp
	out := cp
	return &out, nil
}
func (f *fakeStore) GetPersonConversationByRef(_ context.Context, tenantID, ref string) (*store.PersonConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.convs {
		if c.TenantID == tenantID && c.Ref == ref {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) PersonConversationsWith(_ context.Context, tenantID, kind, peer string, since time.Time) ([]store.PersonConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PersonConversation
	for _, c := range f.convs {
		if c.TenantID == tenantID && c.PeerKind == kind && c.Peer == peer && !c.LastOutAt.Before(since) {
			out = append(out, *c)
		}
	}
	return out, nil
}
func (f *fakeStore) MarkPersonConversationIn(_ context.Context, tenantID, id string, at time.Time, mid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.convs[tenantID+"|"+id]; ok {
		c.LastInAt = &at
		if mid != "" {
			c.LastInMessageID = mid
		}
	}
	return nil
}
func (f *fakeStore) MarkPersonConversationOut(_ context.Context, tenantID, id string, at time.Time, mid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.convs[tenantID+"|"+id]; ok {
		if at.After(c.LastOutAt) {
			c.LastOutAt = at
		}
		if mid != "" {
			c.LastOutMessageID = mid
		}
	}
	return nil
}
func (f *fakeStore) RecordPersonChatSend(_ context.Context, s *store.PersonChatSend) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, *s)
	return nil
}
func (f *fakeStore) conv(tenantID string) *store.PersonConversation {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.convs {
		if c.TenantID == tenantID {
			return c
		}
	}
	return nil
}

type fakeDir map[string][]directory.Contact

func (d fakeDir) ListContacts(_ context.Context, tenantID string) ([]directory.Contact, error) {
	return d[tenantID], nil
}

type fakeTenants struct{ s *fakeStore }

// ForDeviceTopic behaves like tenancy.Resolver: the registered owner wins
// whatever the topic says.
func (f fakeTenants) ForDeviceTopic(_ context.Context, imei, topicTenant string) string {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if owner, ok := f.s.devices[imei]; ok {
		return owner
	}
	return topicTenant
}

type sent struct{ to, body string }

type fakeSMS struct {
	mu   sync.Mutex
	from string
	out  []sent
}

func (f *fakeSMS) Send(_ context.Context, to, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, sent{to, body})
	return nil
}
func (f *fakeSMS) From() string { return f.from }

type fakeMail struct {
	mu   sync.Mutex
	from string
	out  []email.Message
}

func (f *fakeMail) SendMessage(_ context.Context, m email.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, m)
	return "<sent-" + string(rune('a'+len(f.out))) + "@hub>", nil
}
func (f *fakeMail) From() string { return f.from }

type fakeAccounts struct {
	sms     map[string]*fakeSMS
	mail    map[string]*fakeMail
	replyTo string
}

func (a fakeAccounts) SMS(_ context.Context, tenantID string) SMS {
	if s, ok := a.sms[tenantID]; ok {
		return s
	}
	return nil
}
func (a fakeAccounts) Mail(_ context.Context, tenantID string) (Mail, string) {
	if m, ok := a.mail[tenantID]; ok {
		return m, a.replyTo
	}
	return nil, ""
}

type mt struct{ tenant, imei, text string }

type harness struct {
	svc   *Service
	st    *fakeStore
	dir   fakeDir
	sms   map[string]*fakeSMS
	mail  map[string]*fakeMail
	mu    sync.Mutex
	mts   []mt
	clock time.Time
}

const (
	tenantA = "t_alpha"
	victim  = store.DefaultTenantID
	modemA  = "300434060000101"
	modemA2 = "300434060000102"
	modemV  = "300434060000999"
	alice   = "+31612345001"
	bob     = "+31612345002"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{st: newFakeStore(), clock: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	h.st.devices[modemA], h.st.labels[modemA] = tenantA, "Node One"
	h.st.devices[modemA2], h.st.labels[modemA2] = tenantA, "Node Two"
	h.st.devices[modemV] = victim
	h.dir = fakeDir{tenantA: {
		{DisplayName: "Ops Desk", Addresses: []directory.Address{{Kind: directory.KindEmail, Value: "ops@example.org"}}},
		{DisplayName: "Alice", Addresses: []directory.Address{{Kind: directory.KindSMS, Value: alice}}},
	}}
	h.sms = map[string]*fakeSMS{tenantA: {from: "+3197000000001"}, victim: {from: "+31600000999"}}
	h.mail = map[string]*fakeMail{tenantA: {from: "hub@alpha.example"}, victim: {from: "hub@meshsat.example"}}
	h.svc = New(nil, h.st, h.dir, fakeTenants{h.st}, fakeAccounts{sms: h.sms, mail: h.mail, replyTo: "replies@alpha.example"},
		func(_ context.Context, tenantID, imei, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.mts = append(h.mts, mt{tenantID, imei, text})
			return nil
		}, nil, Options{PlatformNumbers: []string{"+31600000999"}})
	h.svc.now = func() time.Time { return h.clock }
	h.svc.later = func(ctx context.Context, f func(context.Context)) { f(ctx) }
	return h
}

func (h *harness) mo(tenantID, imei, id, text string) {
	payload, _ := json.Marshal(map[string]any{"id": id, "channel": "iridium_imt", "text": text})
	h.svc.handle(hubmqtt.TopicSatMOFor(tenantID, imei), payload)
}

func (h *harness) mtsTo(imei string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.mts {
		if m.imei == imei {
			out = append(out, m.text)
		}
	}
	return out
}

func (h *harness) setMode(tenantID, mode string) {
	h.st.tenants[tenantID] = &store.Tenant{ID: tenantID, PersonChatMode: mode}
}

// ---- device -> person ----

func TestContactsModeSendsToADirectoryNumberFromTheTenantsOwnAccount(t *testing.T) {
	h := newHarness(t)
	h.mo(tenantA, modemA, "m1", "@+31612345001 water at the north gate")
	got := h.sms[tenantA].out
	if len(got) != 1 || got[0].to != alice || got[0].body != "Node One: water at the north gate" {
		t.Fatalf("sms = %+v", got)
	}
	if len(h.sms[victim].out) != 0 {
		t.Fatal("the platform's account sent for a customer tenant")
	}
	c := h.st.conv(tenantA)
	if c == nil || c.LastOutAt.IsZero() || c.DeviceID != modemA || c.Peer != alice {
		t.Fatalf("conversation = %+v", c)
	}
	if len(h.mtsTo(modemA)) != 0 {
		t.Fatal("a successful send must not notify the device")
	}
}

func TestContactsModeRefusesAStrangerAndTellsTheDevice(t *testing.T) {
	h := newHarness(t)
	h.mo(tenantA, modemA, "m1", "@+31699999999 hello stranger")
	if len(h.sms[tenantA].out) != 0 {
		t.Fatal("a number outside the directory was texted in contacts mode")
	}
	n := h.mtsTo(modemA)
	if len(n) != 1 || !strings.HasPrefix(n[0], "[MeshSat Hub] not sent to +31699999999: not in this account's contacts") {
		t.Fatalf("notice = %q", n)
	}
	if c := h.st.conv(tenantA); c != nil {
		t.Fatalf("a refused send opened a conversation: %+v", c)
	}
}

func TestOpenModeSendsToAnyNumberAndOffModeToNobody(t *testing.T) {
	h := newHarness(t)
	h.setMode(tenantA, ModeOpen)
	h.mo(tenantA, modemA, "m1", "@+31699999999 hello")
	if len(h.sms[tenantA].out) != 1 {
		t.Fatal("open mode must send to any number")
	}
	h.setMode(tenantA, ModeOff)
	h.mo(tenantA, modemA, "m2", "@+31699999999 again")
	payload, _ := json.Marshal(map[string]any{"id": "m3", "channel": "iridium", "text": "@+31612345001 x"})
	if len(h.sms[tenantA].out) != 1 || h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, modemA), payload) {
		t.Fatal("off mode must send nothing and claim nothing, so routes see the text as before")
	}
}

// The cross-tenant hole the design review found: a bridge of one tenant
// publishes in its own namespace for a modem another tenant owns. The
// victim is the default tenant, deliberately.
func TestAMessageInAnotherTenantsNamespaceForTheVictimsModemDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.setMode(victim, ModeOpen)
	h.mo(tenantA, modemV, "m1", "@+31699999999 send from the victim's account")
	payload, _ := json.Marshal(map[string]any{"id": "m1", "channel": "iridium_imt", "text": "@+31699999999 x"})
	if h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, modemV), payload) {
		t.Fatal("the routing claim accepted a modem published in another tenant's namespace")
	}
	if len(h.sms[victim].out)+len(h.sms[tenantA].out) != 0 || len(h.mtsTo(modemV)) != 0 {
		t.Fatal("a message for another tenant's modem made the Hub send")
	}
}

func TestUnregisteredOpaqueAndNonSatelliteMessagesAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.setMode(tenantA, ModeOpen)
	h.mo(tenantA, "300434069999999", "m1", "@+31699999999 not registered")
	for _, p := range []map[string]any{
		{"id": "m2", "channel": "iridium", "text": "@+31699999999 x", "opaque": true},
		{"id": "m3", "channel": "sms", "text": "@+31699999999 x"},
	} {
		b, _ := json.Marshal(p)
		h.svc.handle(hubmqtt.TopicSatMOFor(tenantA, modemA), b)
	}
	if len(h.sms[tenantA].out) != 0 {
		t.Fatalf("sent %+v", h.sms[tenantA].out)
	}
}

func TestMoDecodedAloneNeverSends(t *testing.T) {
	// Every bridge may publish mo/decoded; only the provider's copy sends.
	h := newHarness(t)
	payload, _ := json.Marshal(map[string]any{"id": "m1", "channel": "iridium", "text": "@+31612345001 x"})
	h.svc.handle(hubmqtt.TopicMODecodedFor(tenantA, modemA), payload)
	if len(h.sms[tenantA].out) != 0 {
		t.Fatal("a message on mo/decoded made the lane send")
	}
	if !h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, modemA), payload) {
		t.Fatal("the same message must still be claimed, so no route sends it either")
	}
}

func TestNameResolvesThroughTheDirectoryAndAmbiguityIsNotGuessed(t *testing.T) {
	h := newHarness(t)
	h.mo(tenantA, modemA, "m1", "@OpsDesk need fuel")
	if got := h.mail[tenantA].out; len(got) != 1 || got[0].To != "ops@example.org" {
		t.Fatalf("mail = %+v", got)
	}
	h.dir[tenantA] = append(h.dir[tenantA], directory.Contact{DisplayName: "ops desk",
		Addresses: []directory.Address{{Kind: directory.KindEmail, Value: "other@example.org"}}})
	payload, _ := json.Marshal(map[string]any{"id": "m2", "channel": "iridium", "text": "@OpsDesk again"})
	if h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, modemA), payload) {
		t.Fatal("a name two contacts answer to must not be claimed")
	}
	payload, _ = json.Marshal(map[string]any{"id": "m3", "channel": "iridium", "text": "@N0CALL hi"})
	if h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, modemA), payload) {
		t.Fatal("an APRS-style @CALL that is not a contact must stay ordinary traffic")
	}
}

func TestMailCarriesTheReferenceReplyToAndRequiresTLS(t *testing.T) {
	h := newHarness(t)
	h.mo(tenantA, modemA, "m1", "@ops@example.org need fuel")
	got := h.mail[tenantA].out
	if len(got) != 1 {
		t.Fatalf("mail = %+v", got)
	}
	m := got[0]
	c := h.st.conv(tenantA)
	if !strings.Contains(m.Subject, "[ref "+c.Ref+"]") || m.ReplyTo != "replies@alpha.example" || !m.RequireTLS || m.AsIs {
		t.Fatalf("mail = %+v", m)
	}
	if !strings.HasPrefix(m.Body, "need fuel") || c.LastOutMessageID == "" {
		t.Fatalf("body %q / out id %q", m.Body, c.LastOutMessageID)
	}
}

func TestEndToEndBodiesAreCarriedUnaltered(t *testing.T) {
	h := newHarness(t)
	frame := sealedFrame(t, "on my way")
	h.mo(tenantA, modemA, "m1", "@+31612345001 "+frame)
	if got := h.sms[tenantA].out; len(got) != 1 || got[0].body != frame {
		t.Fatalf("a sealed SMS frame must go verbatim, got %+v", got)
	}
	_, compact := pgpMessage(t, "the code is 4471")
	h.mo(tenantA, modemA, "m2", "@ops@example.org "+compact)
	got := h.mail[tenantA].out
	if len(got) != 1 || !got[0].AsIs || !strings.HasPrefix(got[0].Body, "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("a compact PGP body must go armoured and never re-encrypted, got %+v", got)
	}
	long := sealedFrame(t, strings.Repeat("x", 120))
	h.mo(tenantA, modemA, "m3", "@+31612345001 "+long)
	if len(h.sms[tenantA].out) != 1 {
		t.Fatal("an encrypted SMS longer than one segment was sent")
	}
	if n := h.mtsTo(modemA); len(n) != 1 || !strings.Contains(n[0], "must fit one message") {
		t.Fatalf("notice = %q", n)
	}
}

func TestTheLaneRefusesItsOwnNumbersAddressesAndAKitsSIM(t *testing.T) {
	h := newHarness(t)
	h.setMode(tenantA, ModeOpen)
	h.st.bridges[tenantA] = []*store.Bridge{{BridgeID: "kit-1"}}
	h.st.peers[tenantA+"|kit-1"] = &store.OOBPeer{Phone: "+31600000777"}
	for i, text := range []string{
		"@+31600000999 the platform number",
		"@+3197000000001 the tenant's own number",
		"@+31600000777 a kit's SIM",
		"@hub@alpha.example the tenant's own From",
		"@replies@alpha.example the tenant's reply address",
	} {
		h.mo(tenantA, modemA, "m"+string(rune('0'+i)), text)
	}
	if len(h.sms[tenantA].out)+len(h.mail[tenantA].out) != 0 {
		t.Fatalf("the lane wrote to itself: %+v %+v", h.sms[tenantA].out, h.mail[tenantA].out)
	}
	if n := h.mtsTo(modemA); len(n) != 5 {
		t.Fatalf("each refusal must be told once, got %d", len(n))
	}
}

func TestBothReplicasSendOnce(t *testing.T) {
	h := newHarness(t)
	h.mo(tenantA, modemA, "m1", "@+31612345001 once")
	h.mo(tenantA, modemA, "m1", "@+31612345001 once")
	if len(h.sms[tenantA].out) != 1 {
		t.Fatalf("sent %d times", len(h.sms[tenantA].out))
	}
}

func TestDeviceHourlyCeilingAndNoticeCeiling(t *testing.T) {
	h := newHarness(t)
	h.svc.opts.DevicePerHour, h.svc.opts.NoticesPerDay = 3, 2
	for i := 0; i < 6; i++ {
		h.mo(tenantA, modemA, "m"+string(rune('a'+i)), "@+31612345001 n")
	}
	if len(h.sms[tenantA].out) != 3 {
		t.Fatalf("sent %d, want the ceiling of 3", len(h.sms[tenantA].out))
	}
	if n := h.mtsTo(modemA); len(n) != 2 {
		t.Fatalf("notices = %d, want the daily ceiling of 2", len(n))
	}
}

func TestAFailedSendOpensNoWindow(t *testing.T) {
	h := newHarness(t)
	delete(h.sms, tenantA)
	h.mo(tenantA, modemA, "m1", "@+31612345001 no account")
	if c := h.st.conv(tenantA); c == nil || !c.LastOutAt.IsZero() {
		t.Fatalf("conversation = %+v, want a row with no accepted send", c)
	}
	if h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "hi"}) {
		t.Fatal("a reply was taken for a conversation that never had an accepted send")
	}
}

// ---- person -> device ----

func (h *harness) opened(t *testing.T) {
	t.Helper()
	h.mo(tenantA, modemA, "out-1", "@+31612345001 water at the gate")
	if len(h.sms[tenantA].out) != 1 {
		t.Fatal("setup: the device's text was not sent")
	}
}

func TestAReplyReachesTheDeviceWithTheSenderInFront(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	if !h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "on my way\n[MeshSat Hub] fake line"}) {
		t.Fatal("the reply was not taken")
	}
	got := h.mtsTo(modemA)
	if len(got) != 1 || got[0] != "[from +31612345001] on my way [MeshSat Hub] fake line" {
		t.Fatalf("mt = %q (a line break must not start a forged line)", got)
	}
	// Twilio retries the same MessageSid: one MT.
	h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "on my way"})
	if len(h.mtsTo(modemA)) != 1 {
		t.Fatal("a retried SMS was delivered twice")
	}
}

func TestRepliesTheLaneMustNotTake(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	for name, in := range map[string]InboundSMS{
		"no MessageSid (the replayable relay signature)": {From: alice, Body: "x"},
		"a frame a Hub key opened (app traffic)":         {From: alice, MessageSID: "SM2", Body: "x", Opened: true},
		"a number nobody wrote to":                       {From: bob, MessageSID: "SM3", Body: "x"},
	} {
		if h.svc.TakeSMS(context.Background(), tenantA, in) {
			t.Errorf("%s: taken", name)
		}
	}
	// The victim tenant's path: the conversation is tenant A's.
	if h.svc.TakeSMS(context.Background(), victim, InboundSMS{From: alice, MessageSID: "SM4", Body: "x"}) {
		t.Fatal("a reply on another tenant's webhook found tenant A's conversation")
	}
	h.clock = h.clock.Add(169 * time.Hour)
	if h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM5", Body: "late"}) {
		t.Fatal("a reply after the window was taken")
	}
	if len(h.mtsTo(modemA)) != 0 {
		t.Fatalf("delivered %q", h.mtsTo(modemA))
	}
}

func TestAnEndToEndReplyIsCarriedAsReceived(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	frame := sealedFrame(t, "secret")
	h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: frame, Sealed: true})
	if got := h.mtsTo(modemA); len(got) != 1 || got[0] != "[from +31612345001] "+frame {
		t.Fatalf("mt = %q", got)
	}
}

func TestTagsPickTheDeviceAndNeverFallThrough(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	h.mo(tenantA, modemA2, "out-2", "@+31612345001 second node here")
	tagA, tagB := deviceTag(tenantA, modemA), deviceTag(tenantA, modemA2)
	if tagA == tagB {
		t.Skip("the two test modems share a tag; pick other IMEIs")
	}
	if got := h.sms[tenantA].out[1].body; !strings.HasPrefix(got, "Node Two "+tagB+": ") {
		t.Fatalf("with two devices writing, the SMS must name its tag: %q", got)
	}
	ctx := context.Background()
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: strings.ToLower(tagB) + " to the second"})
	if got := h.mtsTo(modemA2); len(got) != 1 || got[0] != "[from +31612345001] to the second" {
		t.Fatalf("tagged reply = %q", got)
	}
	// No tag with two open: nobody, and one hint SMS a day.
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: alice, MessageSID: "SM2", Body: "which one?"})
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: alice, MessageSID: "SM3", Body: "which one??"})
	hints := 0
	for _, s := range h.sms[tenantA].out {
		if strings.HasPrefix(s.body, "MeshSat: several devices wrote to you") {
			hints++
		}
	}
	if hints != 1 || len(h.mtsTo(modemA)) != 0 {
		t.Fatalf("hints = %d, mts to A = %q", hints, h.mtsTo(modemA))
	}
	// The second device's conversation closes; a reply tagged for it must not
	// fall through to the one still open.
	h.clock = h.clock.Add(time.Hour)
	h.mo(tenantA, modemA, "out-3", "@+31612345001 still here")
	for k, c := range h.st.convs {
		if c.DeviceID == modemA2 {
			h.st.convs[k].LastOutAt = h.clock.Add(-200 * time.Hour)
		}
	}
	h.svc.TakeSMS(ctx, tenantA, InboundSMS{From: alice, MessageSID: "SM4", Body: tagB + " for the closed one"})
	if got := h.mtsTo(modemA); len(got) != 0 {
		t.Fatalf("a reply tagged for a closed conversation reached another device: %q", got)
	}
}

func TestADeviceNoLongerTheTenantsIsNotMessaged(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	h.st.devices[modemA] = victim // transferred
	h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "hi"})
	if len(h.mtsTo(modemA)) != 0 {
		t.Fatal("a reply went to a modem another tenant owns now")
	}
}

func TestReplyQuotaAndThePersonIsToldOnce(t *testing.T) {
	h := newHarness(t)
	h.svc.opts.RepliesPerHour = 2
	h.opened(t)
	for i := 0; i < 5; i++ {
		h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM" + string(rune('a'+i)), Body: "r"})
	}
	if got := len(h.mtsTo(modemA)); got != 2 {
		t.Fatalf("delivered %d, want the ceiling of 2", got)
	}
	told := 0
	for _, s := range h.sms[tenantA].out {
		if strings.Contains(s.body, "your reply was not sent") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the person was told %d times, want once a day", told)
	}
}

func (h *harness) mailOpened(t *testing.T) *store.PersonConversation {
	t.Helper()
	h.mo(tenantA, modemA, "out-m", "@ops@example.org need fuel")
	c := h.st.conv(tenantA)
	if c == nil || c.LastOutAt.IsZero() {
		t.Fatal("setup: the mail was not sent")
	}
	return c
}

func TestAMailReplyNeedsTheReferenceInTheSubjectAndTheRightSender(t *testing.T) {
	h := newHarness(t)
	c := h.mailOpened(t)
	ctx := context.Background()
	subj := "Re: Node One via MeshSat [ref " + c.Ref + "]"
	cases := []struct {
		name string
		in   email.InboundReply
		want bool // delivered
	}{
		{"reference only in the body", email.InboundReply{From: "ops@example.org", Subject: "Re: hello", Text: "[ref " + c.Ref + "] x", MessageID: "<1>"}, false},
		{"forged From", email.InboundReply{From: "mallory@evil.example", Subject: subj, Text: "x", MessageID: "<2>"}, false},
		{"auto reply", email.InboundReply{From: "ops@example.org", Subject: subj, Text: "away", MessageID: "<3>", AutoReply: true}, false},
		{"domain checks failed", email.InboundReply{From: "ops@example.org", Subject: subj, Text: "x", MessageID: "<4>", AuthFailed: true}, false},
		{"the real reply", email.InboundReply{From: "Ops Desk <ops@EXAMPLE.org>", Subject: subj,
			Text: "Fuel at 14:00.\n\nOn Sat, 24 Oct 2026, Node One wrote:\n> need fuel", MessageID: "<5>"}, true},
	}
	for _, tc := range cases {
		before := len(h.mtsTo(modemA))
		h.svc.TakeMailReply(ctx, tenantA, tc.in)
		if delivered := len(h.mtsTo(modemA)) > before; delivered != tc.want {
			t.Errorf("%s: delivered = %v", tc.name, delivered)
		}
	}
	if got := h.mtsTo(modemA); len(got) != 1 || got[0] != "[from ops@example.org] Fuel at 14:00." {
		t.Fatalf("mt = %q", got)
	}
	if c2, _ := h.st.GetPersonConversationByRef(ctx, tenantA, c.Ref); c2.LastInMessageID != "<5>" {
		t.Fatalf("the reply's Message-ID must thread the next mail: %+v", c2)
	}
	// The same ref on the victim tenant's webhook finds nothing.
	if h.svc.TakeMailReply(ctx, victim, email.InboundReply{From: "ops@example.org", Subject: subj, Text: "x", MessageID: "<6>"}) {
		t.Fatal("a reference was honoured on another tenant's webhook")
	}
	// A provider retry of the same Message-ID is one MT.
	h.svc.TakeMailReply(ctx, tenantA, cases[4].in)
	if len(h.mtsTo(modemA)) != 1 {
		t.Fatal("a redelivered mail was sent twice")
	}
}

func TestAnEncryptedMailReplyGoesToTheDeviceCompact(t *testing.T) {
	h := newHarness(t)
	h.svc.opts.SBDMaxBytes = 2048
	c := h.mailOpened(t)
	arm, compact := pgpMessage(t, "the code is 4471")
	h.svc.TakeMailReply(context.Background(), tenantA, email.InboundReply{From: "ops@example.org",
		Subject: "Re: x [ref " + c.Ref + "]", PGP: arm, MessageID: "<p1>"})
	if got := h.mtsTo(modemA); len(got) != 1 || got[0] != "[from ops@example.org] "+compact {
		t.Fatalf("mt = %q", got)
	}
}

func TestAnEncryptedReplyTooLongForTheLinkIsNotCut(t *testing.T) {
	h := newHarness(t)
	c := h.mailOpened(t)
	arm, _ := pgpMessage(t, strings.Repeat("long ", 80))
	h.svc.TakeMailReply(context.Background(), tenantA, email.InboundReply{From: "ops@example.org",
		Subject: "Re: x [ref " + c.Ref + "]", PGP: arm, MessageID: "<p2>"})
	if len(h.mtsTo(modemA)) != 0 {
		t.Fatal("an end-to-end reply longer than an SBD MT was sent (it would have been cut)")
	}
	found := false
	for _, m := range h.mail[tenantA].out {
		if strings.Contains(m.Body, "your encrypted reply is") {
			found = true
		}
	}
	if !found {
		t.Fatal("the person was not told their encrypted reply did not fit")
	}
}

func TestClaimsTheMarkedReply(t *testing.T) {
	h := newHarness(t)
	payload, _ := json.Marshal(map[string]any{"id": "sms-in-SM1", "channel": "sms", "text": "on my way", "lane": LaneMarker})
	if !h.svc.Claims(hubmqtt.TopicMODecodedFor(tenantA, alice), payload) {
		t.Fatal("a reply the webhook marked must be claimed, so no route relays it")
	}
}

func TestAKitWithNoSIMIsNoReasonToRefuseButAFailedLookupIs(t *testing.T) {
	h := newHarness(t)
	h.st.bridges[tenantA] = []*store.Bridge{{BridgeID: "kit-no-sim"}}
	h.mo(tenantA, modemA, "m1", "@+31612345001 hello")
	if len(h.sms[tenantA].out) != 1 {
		t.Fatal("a kit with no pairing on file blocked an ordinary send")
	}
	h.st.peerErr = errors.New("database down")
	h.mo(tenantA, modemA, "m2", "@+31612345001 again")
	if len(h.sms[tenantA].out) != 1 {
		t.Fatal("a failed kit lookup let the send through: it could have been a kit's SIM")
	}
}

func TestAClaimErrorLeavesTheSMSToThePipeline(t *testing.T) {
	h := newHarness(t)
	h.opened(t)
	h.st.claimErr = errors.New("database down")
	if h.svc.TakeSMS(context.Background(), tenantA, InboundSMS{From: alice, MessageSID: "SM1", Body: "hi"}) {
		t.Fatal("a reply was marked as the lane's while the lane could not deliver it")
	}
}

func TestCleanMessageID(t *testing.T) {
	for in, want := range map[string]string{
		"<abc.123@example.org>":       "<abc.123@example.org>",
		" <abc@example.org> ":         "<abc@example.org>",
		"<abc@example.org>\r\nBcc: x": "",
		"abc@example.org":             "",
		"<a b@example.org>":           "",
	} {
		if got := cleanMessageID(in); got != want {
			t.Errorf("cleanMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}

func (f *fakeStore) UpsertTAKContact(context.Context, *store.TAKContact) error { return nil }
func (f *fakeStore) GetTAKContact(context.Context, string, string) (*store.TAKContact, error) {
	return nil, nil
}
func (f *fakeStore) ListTAKContacts(context.Context, string, time.Time) ([]store.TAKContact, error) {
	return nil, nil
}
func (f *fakeStore) SaveTAKChatMessage(context.Context, *store.TAKChatMessage) (bool, error) {
	return false, nil
}
func (f *fakeStore) SetTAKChatMessageStatus(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeStore) SetTAKChatMessageOutcome(context.Context, string, string, string, string, string) (*store.TAKChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) GetTAKChatMessage(context.Context, string, string) (*store.TAKChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) ListTAKChatMessages(context.Context, string, string, int) ([]store.TAKChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) UpsertMatrixOperator(context.Context, *store.MatrixOperator) error { return nil }
func (f *fakeStore) GetMatrixOperator(context.Context, string, string) (*store.MatrixOperator, error) {
	return nil, nil
}
func (f *fakeStore) GetMatrixSync(context.Context, string) (*store.MatrixSync, error) {
	return nil, nil
}
func (f *fakeStore) SaveMatrixSync(context.Context, *store.MatrixSync) error     { return nil }
func (f *fakeStore) UpsertWarning(context.Context, *store.Warning) (bool, error) { return true, nil }
func (f *fakeStore) GetWarning(context.Context, string, string) (*store.Warning, error) {
	return nil, nil
}
func (f *fakeStore) UpsertWarningMatch(context.Context, *store.WarningMatch) error       { return nil }
func (f *fakeStore) SetWarningRaw(context.Context, string, string, string) error         { return nil }
func (f *fakeStore) MarkWarningSuperseded(context.Context, string, string, string) error { return nil }
func (f *fakeStore) ListWarningsInForce(context.Context, string, time.Time) ([]store.TenantWarning, error) {
	return nil, nil
}
func (f *fakeStore) ListTenantWarnings(context.Context, string, int) ([]store.TenantWarning, error) {
	return nil, nil
}
