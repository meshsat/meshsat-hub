package api

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshsat/meshsat-hub/internal/auth"
)

// wsUpgrader checks the Origin header against allowed origins.
// Controlled by HUB_WS_ALLOWED_ORIGINS env var (comma-separated, default: same-origin).
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		allowed := os.Getenv("HUB_WS_ALLOWED_ORIGINS")
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if allowed == "" {
			// Default: same-origin only — compare parsed hostname, not substring.
			return u.Host == r.Host || u.Hostname() == r.Host
		}
		for _, o := range strings.Split(allowed, ",") {
			o = strings.TrimSpace(o)
			if o == "*" {
				return true
			}
			// Compare the origin's hostname against the allowed value.
			if u.Host == o || u.Hostname() == o {
				return true
			}
		}
		return false
	},
}

// WSTokenFromQuery is middleware that copies a ?token= query parameter into
// the Authorization header. This allows browser WebSocket clients (which
// cannot send custom headers on upgrade) to authenticate via query param.
// Must be applied BEFORE the auth middleware chain.
func WSTokenFromQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if token := r.URL.Query().Get("token"); token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// wsWriteTimeout bounds a single frame write. Without it one client that stops
// reading stalls the whole fan-out, because every delivery walks the same list.
const wsWriteTimeout = 5 * time.Second

// wsClient is one connected browser, and the tenant it is allowed to hear from.
type wsClient struct {
	conn     *websocket.Conn
	tenantID string
	// mu serialises writes to this connection. gorilla/websocket permits only
	// one concurrent writer per connection, and two subscriptions delivering at
	// once would otherwise corrupt the frame stream.
	mu sync.Mutex
}

func (c *wsClient) write(msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// WSHub manages WebSocket connections for real-time event streaming.
//
// Every client is bound to the tenant it authenticated as, and an event is
// delivered only to clients of the tenant that owns it. This is not a
// refinement: the hub used to write every frame to every connection, so any
// authenticated viewer of any tenant received every other tenant's positions,
// messages and SOS events. Deliver by tenant or do not deliver.
type WSHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]*wsClient
}

// NewWSHub creates a WebSocket hub.
func NewWSHub() *WSHub {
	return &WSHub{clients: make(map[*websocket.Conn]*wsClient)}
}

// HandleWS upgrades an HTTP connection to WebSocket.
// Authentication is handled by the middleware chain; the WSTokenFromQuery
// middleware copies ?token= into the Authorization header for browser clients.
// @Summary      WebSocket event stream
// @Description  Real-time stream of this tenant's messages, positions and alerts. Pass token via ?token= query param or Authorization header.
// @Tags         websocket
// @Param        token query string false "Auth token (JWT or API key)"
// @Router       /api/ws [get]
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Reject unauthenticated connections.
	if auth.FromContext(r.Context()) == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// The tenant is fixed at upgrade time and never re-read: a long-lived
	// socket must not follow a tenant switch made after it was opened.
	//
	// The raw context value, deliberately, not auth.TenantIDFromContext: that
	// helper answers "default" for a request that carries no tenant, and a
	// socket that quietly joined the platform tenant's event stream is exactly
	// the leak this file exists to prevent. Absent means refused here.
	tenantID, _ := r.Context().Value(auth.TenantContextKey).(string)
	if tenantID == "" {
		http.Error(w, `{"error":"tenant required"}`, http.StatusForbidden)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws: upgrade failed", "error", err)
		return
	}

	c := &wsClient{conn: conn, tenantID: tenantID}
	h.mu.Lock()
	h.clients[conn] = c
	h.mu.Unlock()

	slog.Debug("ws: client connected", "remote", conn.RemoteAddr(), "tenant", tenantID)

	// Read loop — just drain (we only push events).
	go func() {
		defer func() {
			h.remove(conn)
			_ = conn.Close()
			slog.Debug("ws: client disconnected", "remote", conn.RemoteAddr(), "tenant", tenantID)
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()
}

func (h *WSHub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

// BroadcastTenant sends a JSON message to the connected clients of one tenant,
// and to nobody else. An empty tenantID delivers to nobody rather than to
// everybody: a caller that could not work out whose event this is must not be
// able to leak it to every tenant by accident.
func (h *WSHub) BroadcastTenant(tenantID string, msg []byte) {
	if tenantID == "" {
		slog.Warn("ws: refusing to deliver an event with no tenant")
		return
	}

	// Snapshot under the read lock, then write outside it. Writing while
	// holding the lock made a slow client block every other delivery, and the
	// old code deleted from the map under an RLock, which is a concurrent map
	// write and an unrecoverable panic any client could trigger.
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for _, c := range h.clients {
		if c.tenantID == tenantID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		if err := c.write(msg); err != nil {
			slog.Debug("ws: write failed, closing", "error", err, "tenant", tenantID)
			_ = c.conn.Close() // the read loop's defer removes it from the map
		}
	}
}

// ClientCount returns the number of connected WebSocket clients.
func (h *WSHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// TenantClientCount returns how many clients of one tenant are connected.
// Used by the tests that pin the isolation guarantee.
func (h *WSHub) TenantClientCount(tenantID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, c := range h.clients {
		if c.tenantID == tenantID {
			n++
		}
	}
	return n
}
