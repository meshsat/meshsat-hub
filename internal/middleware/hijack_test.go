package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// A WebSocket upgrade must survive the whole wrapper chain the router
// applies (metrics, then logging). Both used to embed the ResponseWriter
// interface and so hid Hijack: every upgrade on production answered 500.
func TestAWebSocketUpgradeSurvivesTheMiddlewareChain(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	h := metrics.ChiMiddleware(Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade wrote its own error
		}
		defer func() { _ = c.Close() }()
		_ = c.WriteMessage(websocket.TextMessage, []byte("hi"))
	})))
	srv := httptest.NewServer(h)
	defer srv.Close()
	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("upgrade through the chain failed: %v (HTTP %d)", err, code)
	}
	defer func() { _ = c.Close() }()
	if _, msg, err := c.ReadMessage(); err != nil || string(msg) != "hi" {
		t.Fatalf("read after upgrade: %q %v", msg, err)
	}
}
