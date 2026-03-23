package api

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/gorilla/websocket"
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
		if allowed == "" {
			// Default: same-origin only.
			return strings.Contains(origin, r.Host)
		}
		for _, o := range strings.Split(allowed, ",") {
			o = strings.TrimSpace(o)
			if o == "*" || strings.Contains(origin, o) {
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

// WSHub manages WebSocket connections for real-time event streaming.
type WSHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

// NewWSHub creates a WebSocket hub.
func NewWSHub() *WSHub {
	return &WSHub{clients: make(map[*websocket.Conn]bool)}
}

// HandleWS upgrades an HTTP connection to WebSocket.
// Authentication is handled by the middleware chain; the WSTokenFromQuery
// middleware copies ?token= into the Authorization header for browser clients.
// @Summary      WebSocket event stream
// @Description  Real-time stream of messages, positions, and alerts. Pass token via ?token= query param or Authorization header.
// @Tags         websocket
// @Param        token query string false "Auth token (JWT or API key)"
// @Router       /api/ws [get]
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Reject unauthenticated connections.
	if auth.FromContext(r.Context()) == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws: upgrade failed", "error", err)
		return
	}

	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	slog.Debug("ws: client connected", "remote", conn.RemoteAddr())

	// Read loop — just drain (we only push events).
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, conn)
			h.mu.Unlock()
			_ = conn.Close()
			slog.Debug("ws: client disconnected", "remote", conn.RemoteAddr())
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()
}

// Broadcast sends a JSON message to all connected WebSocket clients.
func (h *WSHub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for conn := range h.clients {
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			slog.Debug("ws: write failed, closing", "error", err)
			_ = conn.Close()
			delete(h.clients, conn)
		}
	}
}

// ClientCount returns the number of connected WebSocket clients.
func (h *WSHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
