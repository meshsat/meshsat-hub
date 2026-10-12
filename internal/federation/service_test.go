package federation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// --- fakes ---

type fakeAccounts struct {
	mu   sync.Mutex
	rows map[string]map[string]string // tenant -> fields
}

func (f *fakeAccounts) ForTenant(_ context.Context, tenantID, provider string) (*integrations.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fields, ok := f.rows[tenantID]
	if !ok || provider != integrations.ProviderFederation {
		return nil, nil
	}
	cp := map[string]string{}
	for k, v := range fields {
		cp[k] = v
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: cp}, nil
}

func (f *fakeAccounts) Set(_ context.Context, tenantID, provider string, fields map[string]string) (*integrations.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[tenantID]
	if row == nil {
		row = map[string]string{}
		f.rows[tenantID] = row
	}
	for k, v := range fields {
		row[k] = v
	}
	if row["relay_token"] == "" {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		row["relay_token"] = hex.EncodeToString(b)
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: row}, nil
}

type fakeStore struct {
	mu      sync.Mutex
	peers   []store.FederationPeer
	grants  []store.FederationGrant
	devices map[string]string // imei -> tenant
	claims  map[string]bool
}

func (f *fakeStore) GetFederationPeer(_ context.Context, tenantID, id string) (*store.FederationPeer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.peers {
		if p.TenantID == tenantID && p.ID == id {
			cp := p
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetFederationPeerByFingerprint(_ context.Context, tenantID, fp string) (*store.FederationPeer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.peers {
		if p.TenantID == tenantID && p.Fingerprint == fp {
			cp := p
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) HasFederationGrant(_ context.Context, tenantID, peerID, from, to string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.grants {
		if g.TenantID == tenantID && g.PeerID == peerID && g.FromIMEI == from && g.ToIMEI == to {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) GetDevice(_ context.Context, tenantID, imei string) (*store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices[imei] != tenantID {
		return nil, store.ErrNotFound
	}
	return &store.Device{IMEI: imei}, nil
}

func (f *fakeStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}

type mtLog struct {
	mu       sync.Mutex
	sent     []string
	failNext int // fail this many sends before succeeding
}

func (m *mtLog) send(_, imei, wire, topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		return errors.New("satellite provider down")
	}
	m.sent = append(m.sent, imei+"|"+wire+"|"+topic)
	return nil
}

func (m *mtLog) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// --- harness ---

const (
	senderIMEI = "300434060000001"
	kitIMEI    = "300434060000002"
)

type world struct {
	sender, receiver    *Service
	sendStore, rcvStore *fakeStore
	rcvAccounts         *fakeAccounts
	mt                  *mtLog
	srv                 *httptest.Server
	peerOnSender        string // id of the receiver as a peer of the sender
	peerOnReceiver      string // id of the sender as a peer of the receiver
}

// newWorld: tenant "t-sender" on one Hub relays to the DEFAULT tenant (the
// victim) on another. The receiving Hub resolves the tenant from the token
// the way internal/webhookroute does.
func newWorld(t *testing.T, receiverAddsSender, grant bool) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{mt: &mtLog{}}
	w.sendStore = &fakeStore{devices: map[string]string{senderIMEI: "t-sender"}, claims: map[string]bool{}}
	w.rcvStore = &fakeStore{devices: map[string]string{kitIMEI: store.DefaultTenantID}, claims: map[string]bool{}}
	sendAccounts := &fakeAccounts{rows: map[string]map[string]string{}}
	w.rcvAccounts = &fakeAccounts{rows: map[string]map[string]string{}}
	w.sender = New(w.sendStore, sendAccounts, "https://sender.example", func(string, string, string, string) error { return nil }, nil)
	w.receiver = New(w.rcvStore, w.rcvAccounts, "https://receiver.example", w.mt.send, nil)

	rcvID, _, err := w.receiver.Identity(ctx, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	sndID, _, err := w.sender.Identity(ctx, "t-sender")
	if err != nil {
		t.Fatal(err)
	}
	token := w.rcvAccounts.rows[store.DefaultTenantID]["relay_token"]
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.URL.Path, "/api/webhook/federation/")
		tenant := ""
		for tid, row := range w.rcvAccounts.rows {
			if row["relay_token"] == tok {
				tenant = tid
			}
		}
		if tenant == "" {
			http.NotFound(rw, r) // what internal/webhookroute answers for an unknown token
			return
		}
		w.receiver.ServeHTTP(rw, r.WithContext(webhookroute.NewContext(r.Context(), &webhookroute.Resolved{TenantID: tenant})))
	}))
	t.Cleanup(w.srv.Close)
	w.sender.DisableNetguardForTest(w.srv.Client())

	w.peerOnSender = "peer-rcv"
	w.sendStore.peers = append(w.sendStore.peers, store.FederationPeer{ID: w.peerOnSender, TenantID: "t-sender",
		RelayURL: w.srv.URL + "/api/webhook/federation/" + token, CertPEM: string(rcvID.CertPEM), Fingerprint: rcvID.Fingerprint})
	w.peerOnReceiver = "peer-snd"
	if receiverAddsSender {
		w.rcvStore.peers = append(w.rcvStore.peers, store.FederationPeer{ID: w.peerOnReceiver, TenantID: store.DefaultTenantID,
			RelayURL: "https://sender.example/api/webhook/federation/x", CertPEM: string(sndID.CertPEM), Fingerprint: sndID.Fingerprint})
	}
	if grant {
		w.rcvStore.grants = append(w.rcvStore.grants, store.FederationGrant{ID: "g1", TenantID: store.DefaultTenantID,
			PeerID: w.peerOnReceiver, FromIMEI: senderIMEI, ToIMEI: kitIMEI})
	}
	return w
}

func wire() string { return base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03, 0x04}) }

func (w *world) relay(t *testing.T, msgID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return w.sender.Relay(ctx, "t-sender", w.peerOnSender, senderIMEI, kitIMEI, wire(), "IMT_TOPIC_RAW", msgID)
}

// --- tests ---

func TestRelayAcrossPeeredHubsSendsOnce(t *testing.T) {
	w := newWorld(t, true, true)
	if err := w.relay(t, "mo-1"); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if w.mt.count() != 1 || w.mt.sent[0] != kitIMEI+"|"+wire()+"|IMT_TOPIC_RAW" {
		t.Fatalf("MT sends: %v", w.mt.sent)
	}
	// The same message again (a retry, or a replay inside the date window).
	if err := w.relay(t, "mo-1"); err != nil {
		t.Fatalf("duplicate relay should answer OK: %v", err)
	}
	if w.mt.count() != 1 {
		t.Fatalf("a duplicate was sent again: %v", w.mt.sent)
	}
	if st := w.receiver.Snapshot(); st.Received != 1 || st.Duplicate != 1 {
		t.Fatalf("receiver stats %+v", st)
	}
}

func TestRelayRefusedWithoutMutualPeering(t *testing.T) {
	w := newWorld(t, false, false)
	if err := w.relay(t, "mo-2"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("one-sided peering: %v", err)
	}
	if w.mt.count() != 0 {
		t.Fatalf("MT sent without peering")
	}
}

func TestRelayRefusedWithoutGrant(t *testing.T) {
	w := newWorld(t, true, false)
	if err := w.relay(t, "mo-3"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("no grant: %v", err)
	}
	if w.mt.count() != 0 {
		t.Fatalf("MT sent without a grant")
	}
}

func TestRelayRefusedForADeviceTheTenantDoesNotOwn(t *testing.T) {
	w := newWorld(t, true, true)
	w.rcvStore.devices[kitIMEI] = "someone-else"
	if err := w.relay(t, "mo-4"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign device: %v", err)
	}
	if w.mt.count() != 0 {
		t.Fatalf("MT sent to a device of another tenant")
	}
}

func TestRelayRefusesAForgedSender(t *testing.T) {
	w := newWorld(t, true, true)
	// A third party with its own key claims to be the peered sender.
	impostor := New(&fakeStore{claims: map[string]bool{}}, &fakeAccounts{rows: map[string]map[string]string{}}, "", nil, nil)
	impostor.DisableNetguardForTest(w.srv.Client())
	id, _, err := impostor.Identity(context.Background(), "t-sender-impostor")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"msg_id":"mo-5","attempt":1,"from_imei":"` + senderIMEI + `","to_imei":"` + kitIMEI + `","wire_b64":"` + wire() + `"}`)
	peer, _ := w.sendStore.GetFederationPeer(context.Background(), "t-sender", w.peerOnSender)
	req, _ := http.NewRequest(http.MethodPost, peer.RelayURL, strings.NewReader(string(body)))
	Sign(req, body, id, peer.Fingerprint, time.Now())
	sndFP := w.rcvStore.peers[0].Fingerprint
	req.Header.Set(HeaderFrom, sndFP) // pretend to be the real peer
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// A forged signature answers like an unknown sender: 404.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged sender: %d", resp.StatusCode)
	}
	if w.mt.count() != 0 {
		t.Fatalf("MT sent on a forged request")
	}
}

func TestRelayToAnUnknownTokenIs404(t *testing.T) {
	w := newWorld(t, true, true)
	w.sendStore.peers[0].RelayURL = w.srv.URL + "/api/webhook/federation/not-a-token"
	if err := w.relay(t, "mo-6"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown token: %v", err)
	}
	if w.mt.count() != 0 {
		t.Fatalf("MT sent for an unknown token")
	}
}

func TestIdentityIsCreatedOnceAndKept(t *testing.T) {
	acc := &fakeAccounts{rows: map[string]map[string]string{}}
	s := New(&fakeStore{claims: map[string]bool{}}, acc, "https://hub.example/", nil, nil)
	a, urlA, err := s.Identity(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	b, urlB, err := s.Identity(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != b.Fingerprint || urlA != urlB || !strings.HasPrefix(urlA, "https://hub.example/api/webhook/federation/") {
		t.Fatalf("identity changed or URL wrong: %s %s %s", a.Fingerprint, b.Fingerprint, urlA)
	}
}

func TestRelayRetryAfterAFailedSendIsSent(t *testing.T) {
	w := newWorld(t, true, true)
	w.mt.failNext = 1 // the first MT attempt fails at the provider
	if err := w.relay(t, "mo-7"); err != nil {
		t.Fatalf("relay should succeed on the retry: %v", err)
	}
	if w.mt.count() != 1 {
		t.Fatalf("want exactly one MT after the retry, got %v", w.mt.sent)
	}
	if st := w.receiver.Snapshot(); st.Failed != 1 || st.Received != 1 {
		t.Fatalf("receiver stats %+v", st)
	}
}

func TestRelayFromATenantWithoutIdentityFails(t *testing.T) {
	st := &fakeStore{claims: map[string]bool{}}
	st.peers = []store.FederationPeer{{ID: "p", TenantID: "t-new", RelayURL: "https://peer.example/api/webhook/federation/x", Fingerprint: "fp"}}
	acc := &fakeAccounts{rows: map[string]map[string]string{}}
	s := New(st, acc, "", nil, nil)
	s.DisableNetguardForTest(http.DefaultClient)
	err := s.Relay(context.Background(), "t-new", "p", senderIMEI, kitIMEI, wire(), "", "mo-8")
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("relay without an identity: %v", err)
	}
	if _, ok := acc.rows["t-new"]; ok {
		t.Fatalf("relay created an identity")
	}
}

func TestConcurrentIdentityCreationAgrees(t *testing.T) {
	acc := &fakeAccounts{rows: map[string]map[string]string{}}
	st := &fakeStore{claims: map[string]bool{}}
	a := New(st, acc, "https://hub.example", nil, nil)
	b := New(st, acc, "https://hub.example", nil, nil)
	var wg sync.WaitGroup
	fps := make([]string, 2)
	for i, s := range []*Service{a, b} {
		wg.Add(1)
		go func(i int, s *Service) {
			defer wg.Done()
			id, _, err := s.Identity(context.Background(), "t-race")
			if err != nil {
				t.Errorf("identity %d: %v", i, err)
				return
			}
			fps[i] = id.Fingerprint
		}(i, s)
	}
	wg.Wait()
	stored, _, err := a.LoadIdentityFor(context.Background(), "t-race")
	if err != nil {
		t.Fatal(err)
	}
	if fps[0] != stored.Fingerprint || fps[1] != stored.Fingerprint {
		t.Fatalf("callers got %v, stored %s", fps, stored.Fingerprint)
	}
}
