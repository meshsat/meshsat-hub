package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/relay"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

type relayNoBus struct{}

func (relayNoBus) Connect() error                                                { return nil }
func (relayNoBus) IsConnected() bool                                             { return true }
func (relayNoBus) Disconnect()                                                   {}
func (relayNoBus) Publish(string, byte, bool, []byte) error                      { return nil }
func (relayNoBus) PublishJSON(string, byte, bool, any) error                     { return nil }
func (relayNoBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (relayNoBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }

// relayFixture: tenant acme owns kit-a and phone-1; tenant other owns kit-b.
// Every bridge's password is its id followed by "-pw".
func relayFixture(t *testing.T, opt relay.Options) *httptest.Server {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tn := range []string{"acme", "other"} {
		if err := db.CreateTenant(ctx, &store.Tenant{ID: tn, Slug: tn, Name: tn, Plan: "beta", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []struct{ tenant, id string }{{"acme", "kit-a"}, {"acme", "phone-1"}, {"other", "kit-b"}} {
		if err := db.CreateOrUpdateBridge(ctx, b.tenant, &store.Bridge{BridgeID: b.id, TenantID: b.tenant, Label: b.id}); err != nil {
			t.Fatal(err)
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(b.id+"-pw"), bcrypt.MinCost)
		if err := db.SetBridgeCredentials(ctx, b.tenant, b.id, b.id, string(hash)); err != nil {
			t.Fatal(err)
		}
	}
	rl := relay.New(relayNoBus{}, nil, opt)
	h := NewRelayHandler(db, tenancy.NewResolver(db, store.DefaultTenantID, 0), rl)
	r := chi.NewRouter()
	r.Get("/api/relay/serve", h.Serve)
	r.Get("/api/relay/connect/{bridge_id}", h.Connect)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func basic(user, pass string) http.Header {
	return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}}
}

func wsDial(t *testing.T, srv *httptest.Server, path string, hdr http.Header) (*websocket.Conn, int) {
	t.Helper()
	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, hdr)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if resp == nil {
		t.Fatalf("no response: %v", err)
	}
	return c, resp.StatusCode
}

func TestRelayRefusesWrongOrMissingCredentials(t *testing.T) {
	srv := relayFixture(t, relay.Options{})
	for name, hdr := range map[string]http.Header{
		"no auth":        nil,
		"wrong password": basic("kit-a", "nope"),
		"unknown bridge": basic("ghost", "ghost-pw"),
		"no credentials": basic("kit-b", ""),
	} {
		if _, code := wsDial(t, srv, "/api/relay/serve", hdr); code != http.StatusUnauthorized {
			t.Errorf("%s: serve answered %d, want 401", name, code)
		}
		if _, code := wsDial(t, srv, "/api/relay/connect/kit-a", hdr); code != http.StatusUnauthorized {
			t.Errorf("%s: connect answered %d, want 401", name, code)
		}
	}
}

func TestRelayAcceptsABridgesOwnCredentials(t *testing.T) {
	srv := relayFixture(t, relay.Options{})
	if c, code := wsDial(t, srv, "/api/relay/serve", basic("kit-a", "kit-a-pw")); c == nil || code != http.StatusSwitchingProtocols {
		t.Fatalf("serve: %d", code)
	}
	if c, code := wsDial(t, srv, "/api/relay/connect/kit-a", basic("phone-1", "phone-1-pw")); c == nil || code != http.StatusSwitchingProtocols {
		t.Fatalf("connect: %d", code)
	}
}

func TestRelayConnectIsConfinedToTheCallersTenant(t *testing.T) {
	srv := relayFixture(t, relay.Options{})
	// phone-1 (acme) reaching for kit-b (other): the same 403 as for a
	// bridge that does not exist, so the answer confirms nothing.
	if _, code := wsDial(t, srv, "/api/relay/connect/kit-b", basic("phone-1", "phone-1-pw")); code != http.StatusForbidden {
		t.Fatalf("other tenant's bridge: %d, want 403", code)
	}
	if _, code := wsDial(t, srv, "/api/relay/connect/nonesuch", basic("phone-1", "phone-1-pw")); code != http.StatusForbidden {
		t.Fatalf("unknown bridge: %d, want 403", code)
	}
	if _, code := wsDial(t, srv, "/api/relay/connect/phone-1", basic("phone-1", "phone-1-pw")); code != http.StatusBadRequest {
		t.Fatalf("self: %d, want 400", code)
	}
}

func TestRelayConnectSpendsTheBudgetAndAnswers429(t *testing.T) {
	srv := relayFixture(t, relay.Options{FramesPerMinute: 1, PongWait: time.Minute})
	if _, code := wsDial(t, srv, "/api/relay/connect/kit-a", basic("phone-1", "phone-1-pw")); code != http.StatusSwitchingProtocols {
		t.Fatalf("first connect: %d", code)
	}
	if _, code := wsDial(t, srv, "/api/relay/connect/kit-a", basic("phone-1", "phone-1-pw")); code != http.StatusTooManyRequests {
		t.Fatalf("second connect in the same minute: %d, want 429", code)
	}
}
