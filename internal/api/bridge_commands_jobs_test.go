package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/cmdjobs"
	"github.com/meshsat/meshsat-hub/internal/oob"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// slowKit is an out-of-band leg whose kit takes its time, and counts how many
// commands were actually put on the air.
type slowKit struct {
	sent    atomic.Int32
	release chan struct{}
}

func (k *slowKit) Send(ctx context.Context, _, _ string, bearer, _ string, _ oob.ArgSpec, _ bool) (*oob.Reply, error) {
	k.sent.Add(1)
	select {
	case <-k.release:
		return &oob.Reply{Bearer: bearer, RC: oob.RCOK, Result: "ok", Body: "u1h", Received: time.Now()}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (k *slowKit) ChooseBearer(_ *store.OOBPeer, via string, _ func(string) bool) (string, error) {
	if via == "" {
		via = oob.BearerSMS
	}
	return via, nil
}
func (k *slowKit) Peer(context.Context, string, string) (*store.OOBPeer, error) {
	return &store.OOBPeer{BridgeID: "bridge-kit-a", Phone: "+31600000001"}, nil
}

func commandRig(t *testing.T) (*BridgeCommandHandler, *slowKit) {
	t.Helper()
	kit := &slowKit{release: make(chan struct{})}
	cmdr := bridge.NewCommander(nil, nil)
	cmdr.SetOOB(kit, func(string) bool { return false })
	h := NewBridgeCommandHandler(&mockStore{bridge: &store.Bridge{BridgeID: "bridge-kit-a", Online: true}}, cmdr)
	h.SetJobs(cmdjobs.New(cmdjobs.NewMemoryKV()))
	return h, kit
}

func postCommand(h *BridgeCommandHandler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/bridges/bridge-kit-a/command", strings.NewReader(body))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", "bridge-kit-a")
	w := httptest.NewRecorder()
	h.SendCommand(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc)))
	return w
}

// What happened on 2026-09-20: the edge hung up on a request idle for 65 s and
// re-sent the POST, three times, and each re-send put another real SMS command
// on the air. A re-sent POST must send nothing and be handed the running job.
func TestAReSentCommandIsNotSentTwice(t *testing.T) {
	h, kit := commandRig(t)
	var wg sync.WaitGroup
	var first *httptest.ResponseRecorder
	wg.Add(1)
	go func() { defer wg.Done(); first = postCommand(h, `{"cmd":"mgmt_ping","via":"sms"}`) }()
	for kit.sent.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// The proxy's retries arrive while the kit has not answered yet.
	for i := 0; i < 3; i++ {
		w := postCommand(h, `{"cmd":"mgmt_ping","via":"sms"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("retry %d: http %d %s, want 202 (running job)", i+1, w.Code, w.Body.String())
		}
	}
	if n := kit.sent.Load(); n != 1 {
		t.Fatalf("the command went on the air %d times; the retries must not send it again", n)
	}

	close(kit.release)
	wg.Wait()
	if first.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", first.Code, first.Body.String())
	}
	// A retry that arrives after the answer gets the answer, still without sending.
	w := postCommand(h, `{"cmd":"mgmt_ping","via":"sms"}`)
	var got commandResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Status != "ok" || got.Bearer != "sms" || kit.sent.Load() != 1 {
		t.Fatalf("late retry: http %d %+v, sent %d", w.Code, got, kit.sent.Load())
	}
	// A different command to the same bridge is its own command.
	kit.release = make(chan struct{})
	close(kit.release)
	if w := postCommand(h, `{"cmd":"mgmt_status","via":"sms"}`); w.Code != http.StatusOK || kit.sent.Load() != 2 {
		t.Fatalf("a different command was swallowed as a retry: http %d, sent %d", w.Code, kit.sent.Load())
	}
}

// async answers at once and the result is there to poll for, so no request is
// ever idle long enough for anything in front of the Hub to give up on it.
func TestAnAsyncCommandAnswersAtOnceAndCanBePolled(t *testing.T) {
	h, kit := commandRig(t)
	w := postCommand(h, `{"cmd":"mgmt_ping","via":"imt","async":true,"request_id":"ui-1"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("async POST: http %d %s", w.Code, w.Body.String())
	}
	var acc commandAccepted
	_ = json.Unmarshal(w.Body.Bytes(), &acc)
	if acc.RequestID != "ui-1" || acc.Status != "pending" || acc.Poll != "/api/bridges/bridge-kit-a/commands/ui-1" {
		t.Fatalf("202 body: %+v", acc)
	}

	poll := func() cmdjobs.Job {
		r := httptest.NewRequest(http.MethodGet, acc.Poll, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", "bridge-kit-a")
		rc.URLParams.Add("request_id", "ui-1")
		rec := httptest.NewRecorder()
		h.GetCommand(rec, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc)))
		var j cmdjobs.Job
		_ = json.Unmarshal(rec.Body.Bytes(), &j)
		return j
	}
	if j := poll(); j.State != cmdjobs.StatePending {
		t.Fatalf("before the kit answers: %+v", j)
	}
	close(kit.release)
	deadline := time.Now().Add(2 * time.Second)
	var j cmdjobs.Job
	for j = poll(); j.State == cmdjobs.StatePending && time.Now().Before(deadline); j = poll() {
		time.Sleep(5 * time.Millisecond)
	}
	var resp commandResponse
	_ = json.Unmarshal(j.Response, &resp)
	if j.State != cmdjobs.StateDone || resp.Status != "ok" || resp.Bearer != "imt" {
		t.Fatalf("after the kit answers: %+v / %+v", j, resp)
	}
	if kit.sent.Load() != 1 {
		t.Fatalf("sent %d times", kit.sent.Load())
	}
}
