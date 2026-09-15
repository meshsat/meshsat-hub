package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// The server's WriteTimeout (15 s in production) cuts the connection under a
// handler that waits longer than that, with no response and no log line:
// the shape of MESHSAT-1164's mgmt_log 502 (a kit's reply over SMS took 21 s).
// A handler that must wait extends its own deadline through
// http.ResponseController, which only reaches the server's writer if every
// wrapper in the chain implements Unwrap. This runs the exact production
// chain (metrics, then logging) against a real server.
func TestAHandlerCanOutliveTheServersWriteTimeoutThroughTheChain(t *testing.T) {
	const timeout = 300 * time.Millisecond
	const wait = 2 * timeout

	slow := func(extend bool) http.Handler {
		return metrics.ChiMiddleware(Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if extend {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Errorf("SetWriteDeadline through the chain: %v", err)
				}
			}
			time.Sleep(wait)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})))
	}
	serve := func(h http.Handler) *httptest.Server {
		srv := httptest.NewUnstartedServer(h)
		srv.Config.WriteTimeout = timeout
		srv.Start()
		t.Cleanup(srv.Close)
		return srv
	}

	// Without the extension the client gets no response at all.
	srv := serve(slow(false))
	if resp, err := http.Get(srv.URL); err == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("the server's WriteTimeout did not cut the slow response (status %d, body %q); the test rig is not exercising the trap", resp.StatusCode, body)
	}

	// With it, the same wait answers 200 through both wrappers.
	srv = serve(slow(true))
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("extended deadline did not survive the chain: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}
