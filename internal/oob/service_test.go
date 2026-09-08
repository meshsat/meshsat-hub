package oob

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

type fakeTransport struct {
	mu      sync.Mutex
	sent    []string
	timeout time.Duration
	fail    bool
}

func (f *fakeTransport) Send(_ context.Context, _ string, _ *store.OOBPeer, text string) error {
	if f.fail {
		return context.DeadlineExceeded
	}
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) Timeout() time.Duration { return f.timeout }

func newService(t *testing.T) (*Service, *fakeTransport, []byte) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i * 7)
	}
	svc := New(db, master, nil, Options{Encrypt: true, MaxPerHour: 3})
	tr := &fakeTransport{timeout: 2 * time.Second}
	svc.RegisterTransport(BearerSMS, tr)
	return svc, tr, vectorKey
}

// The bridge issued the key (bundle path): the Hub is the importer, the kit
// replies as the issuer. A sealed request goes out, the kit's reply comes
// back through HandleInbound and resolves the waiting Send by counter.
func TestSendAndReplyRoundTrip(t *testing.T) {
	svc, tr, key := newService(t)
	ctx := context.Background()
	peerID, err := svc.Pair(ctx, "t1", "tesseract", key, RoleImporter, "+31653618463", "")
	if err != nil || peerID != 38091 {
		t.Fatalf("pair: %d %v", peerID, err)
	}
	if b, err := svc.ChooseBearer(&store.OOBPeer{Phone: "+3160"}, "", nil); err != nil || b != BearerSMS {
		t.Fatalf("choose: %s %v", b, err)
	}
	if _, err := svc.ChooseBearer(&store.OOBPeer{}, "", nil); err == nil {
		t.Fatalf("no bearer accepted")
	}

	done := make(chan *Reply, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_ping", ArgSpec{}, false)
		errs <- err
		done <- r
	}()
	// Wait for the frame to leave, then answer it the way the kit would.
	var sentText string
	for i := 0; i < 50 && sentText == ""; i++ {
		time.Sleep(20 * time.Millisecond)
		tr.mu.Lock()
		if len(tr.sent) > 0 {
			sentText = tr.sent[0]
		}
		tr.mu.Unlock()
	}
	if sentText == "" || !strings.HasPrefix(sentText, "MS:") {
		t.Fatalf("nothing sent: %q", sentText)
	}
	wire, _ := Decode(sentText)
	req, err := Open(wire, key, RoleImporter) // the kit opens with the Hub's role
	if err != nil || req.Cmd != CmdPing || req.Counter != 1 || !req.Enc {
		t.Fatalf("kit could not open the request: %+v %v", req, err)
	}
	reply := Frame{Enc: true, Reply: true, PeerID: req.PeerID, Counter: 9, Cmd: req.Cmd,
		Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: uint16(req.Counter), Seq: 1, Total: 1, Body: []byte("u17h b98A q0")})}
	rw, _ := Seal(reply, key, RoleIssuer)
	if !svc.HandleInbound(ctx, BearerSMS, "+31653618463", "junk before "+Encode(rw)) {
		t.Fatalf("reply not classified as a frame")
	}
	if err := <-errs; err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-done
	if r.RC != RCOK || r.Body != "u17h b98A q0" || r.Counter != 1 || r.Bearer != BearerSMS {
		t.Fatalf("reply: %+v", r)
	}
	// The same reply again is a replay and must be dropped (still classified).
	if !svc.HandleInbound(ctx, BearerSMS, "+31653618463", Encode(rw)) {
		t.Fatalf("replay not classified")
	}
	// Plain text is not a frame.
	if svc.HandleInbound(ctx, BearerSMS, "+31653618463", "hello from tesseract") {
		t.Fatalf("plain text classified as frame")
	}
	// A frame under an unknown key is dropped silently but classified.
	other := make([]byte, 32)
	ow, _ := Seal(Frame{Reply: true, PeerID: 38091, Counter: 10, Cmd: CmdPing, Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: 1, Seq: 1, Total: 1})}, other, RoleIssuer)
	if !svc.HandleInbound(ctx, BearerSMS, "+31653618463", Encode(ow)) {
		t.Fatalf("foreign frame not classified")
	}
}

func TestSendLimitsAndErrors(t *testing.T) {
	svc, tr, key := newService(t)
	ctx := context.Background()
	if _, err := svc.Send(ctx, "t1", "ghost", BearerSMS, "mgmt_ping", ArgSpec{}, true); err != ErrNotPaired {
		t.Fatalf("unpaired: %v", err)
	}
	if _, err := svc.Pair(ctx, "t1", "tesseract", key, RoleIssuer, "+3160", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, "dance", ArgSpec{}, true); err != ErrUnknownCmd {
		t.Fatalf("unknown cmd: %v", err)
	}
	if _, err := svc.Send(ctx, "t1", "tesseract", BearerIMT, "mgmt_ping", ArgSpec{}, true); err == nil {
		t.Fatalf("unregistered bearer accepted")
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_reset", ArgSpec{Target: "cellular", Level: 1}, true); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_ping", ArgSpec{}, true); err != ErrRateLimit {
		t.Fatalf("rate limit: %v", err)
	}
	if len(tr.sent) != 3 {
		t.Fatalf("sent %d", len(tr.sent))
	}
	// Counters advance and the frames carry them.
	w, _ := Decode(tr.sent[2])
	f, err := Open(w, key, RoleIssuer)
	if err != nil || f.Counter != 3 || f.Cmd != CmdReset || len(f.Args) != 2 || f.Args[0] != 0x03 {
		t.Fatalf("third frame: %+v %v", f, err)
	}
	// Timeout without a reply.
	svc2, tr2, _ := newService(t)
	tr2.timeout = 50 * time.Millisecond
	_, _ = svc2.Pair(ctx, "t1", "tesseract", key, RoleIssuer, "+3160", "")
	if _, err := svc2.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_ping", ArgSpec{}, false); err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Fatalf("timeout: %v", err)
	}
}
