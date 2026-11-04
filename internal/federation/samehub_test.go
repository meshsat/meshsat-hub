package federation_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/federation"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// Two tenants on ONE Hub (MESHSAT-1788). The Hub's public name does not
// resolve in a test, just as hub.meshsat.net resolves to a private address
// inside the cluster: the request-forgery guard refuses both, and is NOT
// disabled here. A relay that arrives proves it never went over the network.

const (
	sameHubURL  = "https://hub.example.test"
	sendTenant  = "t-sender"
	modemOfA    = "300434060000011"
	kitOfVictim = "300434060000012"
)

type sentMT struct{ tenant, imei, wire, topic string }

type sameHub struct {
	db       *sqlite.DB
	svc      *federation.Service
	urlA     string
	urlB     string
	peerOnA  *store.FederationPeer // the victim, as a peer of the sender
	mu       sync.Mutex
	mts      []sentMT
	resolved []string // the tenancy the receiver saw BEFORE token resolution
}

func (h *sameHub) sent() []sentMT {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentMT(nil), h.mts...)
}

// newSameHub: tenant t-sender relays to the DEFAULT tenant, the victim, on
// the same Hub, through the real store, the real encrypted accounts and the
// real token resolution. wrap, when set, goes in front of the resolution.
func newSameHub(t *testing.T, victimAddsSender, grant bool, wrap func(http.Handler) http.Handler) *sameHub {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := db.CreateTenant(ctx, &store.Tenant{ID: sendTenant, Slug: sendTenant, Name: sendTenant, Plan: "beta", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := db.CreateDevice(ctx, sendTenant, &store.Device{IMEI: modemOfA, Label: "kit A", Type: "iridium_imt", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("device A: %v", err)
	}
	if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: kitOfVictim, Label: "kit B", Type: "iridium_imt", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("device B: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	accounts := integrations.New(db, key)

	h := &sameHub{db: db}
	h.svc = federation.New(db, accounts, sameHubURL, func(tenantID, imei, wireB64, topic string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.mts = append(h.mts, sentMT{tenantID, imei, wireB64, topic})
		return nil
	}, nil)
	resolve := webhookroute.Middleware(accounts, integrations.ProviderFederation, "relay_token")
	h.svc.SetLocalReceiver(func(next http.Handler) http.Handler {
		inner := resolve(next)
		if wrap != nil {
			inner = wrap(inner)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			h.resolved = append(h.resolved, tenancy.FromContext(r.Context()))
			h.mu.Unlock()
			inner.ServeHTTP(w, r)
		})
	}, func(ctx context.Context, token string) (string, error) {
		tenantID, _, err := accounts.LookupByToken(ctx, integrations.ProviderFederation, "relay_token", token)
		return tenantID, err
	})

	idA, urlA, err := h.svc.Identity(ctx, sendTenant)
	if err != nil {
		t.Fatalf("identity A: %v", err)
	}
	idB, urlB, err := h.svc.Identity(ctx, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("identity B: %v", err)
	}
	h.urlA, h.urlB = urlA, urlB
	if !strings.HasPrefix(urlB, sameHubURL+federation.WebhookPathPrefix) || !h.svc.IsLocalRelayURL(ctx, urlB) {
		t.Fatalf("the victim's own relay URL %q is not recognised as this Hub's", urlB)
	}

	h.peerOnA = &store.FederationPeer{Name: "victim", RelayURL: urlB, CertPEM: string(idB.CertPEM), Fingerprint: idB.Fingerprint}
	if err := db.CreateFederationPeer(ctx, sendTenant, h.peerOnA); err != nil {
		t.Fatal(err)
	}
	if victimAddsSender {
		pb := &store.FederationPeer{Name: "sender", RelayURL: urlA, CertPEM: string(idA.CertPEM), Fingerprint: idA.Fingerprint}
		if err := db.CreateFederationPeer(ctx, store.DefaultTenantID, pb); err != nil {
			t.Fatal(err)
		}
		if grant {
			if err := db.CreateFederationGrant(ctx, store.DefaultTenantID, &store.FederationGrant{PeerID: pb.ID, FromIMEI: modemOfA, ToIMEI: kitOfVictim}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return h
}

func payload() string { return base64.StdEncoding.EncodeToString([]byte{0x01, 0xAA, 0x55}) }

// relay runs the way the route engine runs it: on a context carrying the
// SENDING tenant.
func (h *sameHub) relay(t *testing.T, peerID, msgID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(tenancy.WithTenant(context.Background(), sendTenant), 20*time.Second)
	defer cancel()
	return h.svc.Relay(ctx, sendTenant, peerID, modemOfA, kitOfVictim, payload(), "IMT_TOPIC_RAW", msgID)
}

func TestSameHubRelayIsDeliveredInProcessOnce(t *testing.T) {
	h := newSameHub(t, true, true, nil)
	if err := h.relay(t, h.peerOnA.ID, "mo-1"); err != nil {
		t.Fatalf("relay between two tenants of one Hub: %v", err)
	}
	got := h.sent()
	if len(got) != 1 || got[0] != (sentMT{store.DefaultTenantID, kitOfVictim, payload(), "IMT_TOPIC_RAW"}) {
		t.Fatalf("MT sends: %+v", got)
	}
	if err := h.relay(t, h.peerOnA.ID, "mo-1"); err != nil {
		t.Fatalf("a replay should answer OK: %v", err)
	}
	if n := len(h.sent()); n != 1 {
		t.Fatalf("a replay was sent again: %d MTs", n)
	}
	if st := h.svc.Snapshot(); st.Sent != 2 || st.Received != 1 || st.Duplicate != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// The receiver must learn its tenant from the token, never from the sender:
// the route engine's context carries the SENDING tenant.
func TestSameHubReceiverNeverInheritsTheSendersTenant(t *testing.T) {
	h := newSameHub(t, true, true, nil)
	if err := h.relay(t, h.peerOnA.ID, "mo-ctx"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.resolved) != 1 || h.resolved[0] != "" {
		t.Fatalf("the receiver started with tenancy %q, want none", h.resolved)
	}
}

func TestSameHubRelayStillNeedsTheVictimsPeeringAndGrant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adds, grant bool
		want        string
	}{
		{"one-sided peering", false, false, "404"},
		{"no grant", true, false, "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSameHub(t, tc.adds, tc.grant, nil)
			if err := h.relay(t, h.peerOnA.ID, "mo-2"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a %s refusal, got %v", tc.want, err)
			}
			if n := len(h.sent()); n != 0 {
				t.Fatalf("%d MTs sent", n)
			}
		})
	}
}

func TestSameHubAnotherTenantsTokenWithoutItsTrustIsRefused(t *testing.T) {
	h := newSameHub(t, true, true, nil)
	// The sender points a second peer at its OWN relay URL with the victim's
	// certificate: the token names the sender, who never pinned itself.
	p := &store.FederationPeer{Name: "self", RelayURL: h.urlA, CertPEM: h.peerOnA.CertPEM, Fingerprint: "fp-self"}
	if err := h.db.CreateFederationPeer(context.Background(), sendTenant, p); err != nil {
		t.Fatal(err)
	}
	if err := h.relay(t, p.ID, "mo-3"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("relay to a token whose tenant does not trust the sender: %v", err)
	}
	if n := len(h.sent()); n != 0 {
		t.Fatalf("%d MTs sent", n)
	}
}

// The guard still refuses every private or unresolvable address that is not
// this Hub's own receiving endpoint, exactly as before.
func TestSameHubGuardStillRefusesEveryOtherTarget(t *testing.T) {
	h := newSameHub(t, true, true, nil)
	token := strings.TrimPrefix(h.urlB, sameHubURL+federation.WebhookPathPrefix)
	for _, u := range []string{
		// This Hub's receiving endpoint, but no tenant here holds the token:
		// a URL on another Hub that left HUB_PUBLIC_URL at the hosted
		// default, so it takes the network path and meets the guard.
		sameHubURL + federation.WebhookPathPrefix + "no-tenant-holds-this-token",
		"https://10.255.11.65" + federation.WebhookPathPrefix + token,
		"https://127.0.0.1" + federation.WebhookPathPrefix + token,
		"https://169.254.169.254" + federation.WebhookPathPrefix + token,
		"https://hub.example.test:8443" + federation.WebhookPathPrefix + token,
		"https://hub.example.test.evil.test" + federation.WebhookPathPrefix + token,
		"https://hub.example.test/api/webhook/federation/" + token + "/../../../admin",
		"https://meshsat-hub-main-rw.meshsat-hub-db.svc" + federation.WebhookPathPrefix + token,
	} {
		ctx := context.Background()
		p := &store.FederationPeer{Name: "x", RelayURL: u, CertPEM: h.peerOnA.CertPEM, Fingerprint: "fp-" + u}
		if err := h.db.CreateFederationPeer(ctx, sendTenant, p); err != nil {
			t.Fatal(err)
		}
		if err := h.relay(t, p.ID, "mo-4"); err == nil || !strings.Contains(err.Error(), "peer URL refused") {
			t.Errorf("%s: want the guard's refusal, got %v", u, err)
		}
	}
	if n := len(h.sent()); n != 0 {
		t.Fatalf("%d MTs sent", n)
	}
}

// A panic in the receiver answers 500, as the server's recoverer would: the
// relay retries instead of taking the route goroutine (and the pod) down.
func TestSameHubReceiverPanicIsA500AndRetried(t *testing.T) {
	var calls atomic.Int32
	h := newSameHub(t, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				panic("receiver bug")
			}
			next.ServeHTTP(w, r)
		})
	})
	if err := h.relay(t, h.peerOnA.ID, "mo-5"); err != nil {
		t.Fatalf("the retry after a panic should deliver: %v", err)
	}
	if n := len(h.sent()); n != 1 || calls.Load() != 2 {
		t.Fatalf("%d MTs after %d calls", n, calls.Load())
	}
}

// A panic AFTER the receiver answered 202 is a sent MT: a retry would pay for
// a second one.
func TestSameHubPanicAfterTheAnswerIsNotSentTwice(t *testing.T) {
	var calls atomic.Int32
	h := newSameHub(t, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			next.ServeHTTP(w, r)
			panic("after the answer")
		})
	})
	if err := h.relay(t, h.peerOnA.ID, "mo-6"); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if n := len(h.sent()); n != 1 || calls.Load() != 1 {
		t.Fatalf("%d MTs after %d calls", n, calls.Load())
	}
}

func TestIsLocalRelayURL(t *testing.T) {
	ctx := context.Background()
	owner := func(_ context.Context, token string) (string, error) {
		switch token {
		case "tok", "AbC-_09":
			return "t-holder", nil
		case "broken":
			return "", errors.New("store down")
		}
		return "", nil
	}
	svc := federation.New(nil, nil, sameHubURL+"/", nil, nil)
	if svc.IsLocalRelayURL(ctx, sameHubURL+federation.WebhookPathPrefix+"tok") {
		t.Fatal("a URL counts as local before a receiver is set")
	}
	svc.SetLocalReceiver(func(h http.Handler) http.Handler { return h }, owner)
	for u, want := range map[string]bool{
		"https://hub.example.test/api/webhook/federation/unknown":     false,
		"https://hub.example.test/api/webhook/federation/broken":      false,
		"https://hub.example.test/api/webhook/federation/AbC-_09":     true,
		"https://HUB.Example.TEST/api/webhook/federation/tok":         true,
		"https://hub.example.test:443/api/webhook/federation/tok":     true,
		"http://hub.example.test/api/webhook/federation/tok":          false,
		"https://hub.example.test:8443/api/webhook/federation/tok":    false,
		"https://hub.example.test./api/webhook/federation/tok":        false,
		"https://hub.example.tesK/api/webhook/federation/tok":         false, // Kelvin sign
		"https://user@hub.example.test/api/webhook/federation/tok":    false,
		"https://hub.example.test@10.0.0.1/api/webhook/federation/to": false,
		"https://hub.example.test/api/webhook/federation/":            false,
		"https://hub.example.test/api/webhook/federation/a/b":         false,
		"https://hub.example.test/api/webhook/federation/..":          false,
		"https://hub.example.test/api/webhook/federation/%41bc":       false,
		"https://hub.example.test/api/webhook/federation/a%2Fb":       false,
		"https://hub.example.test/api/webhook/federation/tok?x=1":     false,
		"https://hub.example.test/api/webhook/federation/tok#f":       false,
		"https://hub.example.test//api/webhook/federation/tok":        false,
		"https://hub.example.test/api/webhook/rockblock/tok":          false,
		"https://hub.example.test/x/api/webhook/federation/tok":       false,
		" https://hub.example.test/api/webhook/federation/tok":        false,
	} {
		if got := svc.IsLocalRelayURL(ctx, u); got != want {
			t.Errorf("IsLocalRelayURL(%q) = %v, want %v", u, got, want)
		}
	}
	// A Hub whose public URL is not a bare https origin has no same-Hub path.
	for _, pub := range []string{"", "http://hub.example.test", "https://example.test/hub", "https://u@hub.example.test"} {
		s := federation.New(nil, nil, pub, nil, nil)
		s.SetLocalReceiver(func(h http.Handler) http.Handler { return h }, owner)
		if s.IsLocalRelayURL(ctx, "https://hub.example.test/api/webhook/federation/tok") ||
			s.IsLocalRelayURL(ctx, "https://example.test/hub/api/webhook/federation/tok") {
			t.Errorf("public URL %q: a relay URL counted as local", pub)
		}
	}
}
