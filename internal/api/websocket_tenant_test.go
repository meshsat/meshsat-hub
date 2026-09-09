package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
)

// dial opens an authenticated websocket against the hub as a member of tenantID.
func dial(t *testing.T, hub *WSHub, tenantID string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), hubauth.UserContextKey, &hubauth.User{ID: "u-" + tenantID, Roles: []string{"viewer"}})
		ctx = context.WithValue(ctx, hubauth.TenantContextKey, tenantID)
		hub.HandleWS(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial as %s: %v", tenantID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// read returns the next frame, or "" if none arrives within the window.
func read(t *testing.T, c *websocket.Conn, within time.Duration) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, b, err := c.ReadMessage()
	if err != nil {
		return ""
	}
	return string(b)
}

// A tenant's live event stream is its own. This is the test that must never be
// weakened: before it existed, the hub wrote every frame to every connected
// client, so any authenticated viewer of any tenant received every other
// tenant's positions, decoded messages and SOS events.
func TestWS_EventsReachOnlyTheOwningTenant(t *testing.T) {
	hub := NewWSHub()
	a := dial(t, hub, "t-alpha")
	b := dial(t, hub, "t-bravo")

	// Give both upgrades time to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hub.ClientCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.ClientCount() != 2 {
		t.Fatalf("clients connected = %d, want 2", hub.ClientCount())
	}
	if n := hub.TenantClientCount("t-alpha"); n != 1 {
		t.Fatalf("alpha clients = %d, want 1", n)
	}

	hub.BroadcastTenant("t-alpha", []byte(`{"sos":true,"lat":52.3,"lon":4.9}`))

	if got := read(t, a, time.Second); !strings.Contains(got, `"sos":true`) {
		t.Errorf("the owning tenant did not receive its own SOS event, got %q", got)
	}
	if got := read(t, b, 500*time.Millisecond); got != "" {
		t.Errorf("another tenant received an SOS event that was not theirs: %q", got)
	}
}

// A caller that cannot work out whose event it is must not be able to reach
// everybody by passing the empty string.
func TestWS_EmptyTenantDeliversToNobody(t *testing.T) {
	hub := NewWSHub()
	c := dial(t, hub, "t-alpha")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hub.ClientCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	hub.BroadcastTenant("", []byte(`{"leak":true}`))
	if got := read(t, c, 500*time.Millisecond); got != "" {
		t.Errorf("an untenanted event was delivered: %q", got)
	}
}

// A connection with no tenant on its context is refused rather than defaulted.
func TestWS_NoTenantIsRefused(t *testing.T) {
	hub := NewWSHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), hubauth.UserContextKey, &hubauth.User{ID: "u"})
		hub.HandleWS(w, r.WithContext(ctx))
	}))
	defer srv.Close()
	if _, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil); err == nil {
		t.Error("a socket with no tenant was accepted")
	} else if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// Concurrent delivery must not race the map. The old Broadcast deleted from
// h.clients while holding only an RLock, which is a concurrent map write: a
// fatal, unrecoverable panic that any client could trigger by stalling.
// Run with -race.
func TestWS_ConcurrentBroadcastDoesNotRaceTheMap(t *testing.T) {
	hub := NewWSHub()
	for i := 0; i < 4; i++ {
		dial(t, hub, "t-alpha")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hub.ClientCount() < 4 {
		time.Sleep(10 * time.Millisecond)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				hub.BroadcastTenant("t-alpha", []byte(`{"x":1}`))
			}
		}()
	}
	wg.Wait()
}
