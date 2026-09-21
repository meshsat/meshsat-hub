package oob

import (
	"context"
	"errors"
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
	svc := New(db, master, nil, Options{MaxPerHour: 3})
	tr := &fakeTransport{timeout: 2 * time.Second}
	svc.RegisterTransport(BearerSMS, tr)
	return svc, tr, vectorKey
}

// The bridge issued the key (bundle path): the Hub is the importer, the kit
// replies as the issuer. A sealed request goes out, the kit's reply comes
// back through HandleInbound and resolves the waiting Send by counter.
// waitForSend blocks until the transport has a frame, or the deadline passes.
//
// This replaced `for i := 0; i < 50` with a 20ms sleep -- a ONE SECOND budget
// for an asynchronous send. It is not enough on a loaded shared CI runner, and
// it failed pipeline 53984 with "nothing sent" on a commit that touches neither
// this package nor anything it imports. Worse, missing the window costs about
// 450 seconds rather than one: Send is still in flight on its own goroutine and
// the test binary waits for it, so a flake also pushes the whole job toward its
// limit.
//
// The poll interval is unchanged, so a healthy run is exactly as fast as
// before; only the patience for an unhealthy machine went up.
func waitForSend(t *testing.T, tr *fakeTransport) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		n := len(tr.sent)
		var first string
		if n > 0 {
			first = tr.sent[0]
		}
		tr.mu.Unlock()
		if first != "" {
			return first
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing was sent within 15s; the frame never reached the transport")
	return ""
}

func TestSendAndReplyRoundTrip(t *testing.T) {
	svc, tr, key := newService(t)
	ctx := context.Background()
	peerID, err := svc.Pair(ctx, "t1", "tesseract", key, RoleImporter, "+31600000001", "")
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
	sentText := waitForSend(t, tr)
	if !strings.HasPrefix(sentText, "MS:") {
		t.Fatalf("sent frame is not a MeshSat OOB frame: %q", sentText)
	}
	wire, _ := Decode(sentText)
	req, err := Open(wire, key, RoleImporter) // the kit opens with the Hub's role
	if err != nil || req.Cmd != CmdPing || req.Counter != 1 || !req.Enc {
		t.Fatalf("kit could not open the request: %+v %v", req, err)
	}
	reply := Frame{Enc: true, Reply: true, PeerID: req.PeerID, Counter: 9, Cmd: req.Cmd,
		Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: uint16(req.Counter), Seq: 1, Total: 1, Body: []byte("u17h b98A q0")})}
	rw, _ := Seal(reply, key, RoleIssuer)
	if !svc.HandleInbound(ctx, BearerSMS, "+31600000001", "junk before "+Encode(rw)) {
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
	if !svc.HandleInbound(ctx, BearerSMS, "+31600000001", Encode(rw)) {
		t.Fatalf("replay not classified")
	}
	// Plain text is not a frame.
	if svc.HandleInbound(ctx, BearerSMS, "+31600000001", "hello from tesseract") {
		t.Fatalf("plain text classified as frame")
	}
	// A frame under an unknown key is dropped silently but classified.
	other := make([]byte, 32)
	ow, _ := Seal(Frame{Reply: true, PeerID: 38091, Counter: 10, Cmd: CmdPing, Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: 1, Seq: 1, Total: 1})}, other, RoleIssuer)
	if !svc.HandleInbound(ctx, BearerSMS, "+31600000001", Encode(ow)) {
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
	// A command that needs an argument and gets none is the caller's mistake:
	// it must be recognisable as such, so the API can answer 400 with the
	// reason. mgmt_log with no unit came back as a 500 "internal error".
	if _, err := svc.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_log", ArgSpec{}, true); !errors.Is(err, ErrBadArgs) || !strings.Contains(err.Error(), "log unit") {
		t.Fatalf("mgmt_log with no unit: err = %v, want ErrBadArgs naming the log unit", err)
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

// An OOB frame commands real hardware in the field over bearers that are not
// themselves confidential, so sealing is not a setting. This builds the service
// the way a caller who configures NOTHING gets it -- a zero-value Options -- and
// insists the frame that leaves is still sealed.
//
// The zero value is the whole point. While Options carried an Encrypt bool,
// Options{} meant Encrypt:false, so the careless construction was the insecure
// one and only HUB_OOB_ENCRYPT=true (or the config default) saved it. A knob
// whose unset position is "send my field commands in clear" is not a knob worth
// keeping (MESHSAT-1121).
func TestFramesAreAlwaysSealedEvenWithNoOptionsAtAll(t *testing.T) {
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

	svc := New(db, master, nil, Options{}) // nothing configured, deliberately
	tr := &fakeTransport{timeout: 200 * time.Millisecond}
	svc.RegisterTransport(BearerSMS, tr)

	ctx := context.Background()
	if _, err := svc.Pair(ctx, "t1", "tesseract", vectorKey, RoleImporter, "+31600000001", ""); err != nil {
		t.Fatalf("pair: %v", err)
	}
	// No kit is listening, so Send returns "no reply" -- the frame still left,
	// which is all this test cares about.
	go func() { _, _ = svc.Send(ctx, "t1", "tesseract", BearerSMS, "mgmt_ping", ArgSpec{}, false) }()

	sent := waitForSend(t, tr)

	wire, err := Decode(sent)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	h, err := ParseHeader(wire)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if !h.Enc() {
		t.Fatal("a frame built from a zero-value Options went out UNSEALED: " +
			"the args of a command that can reboot or factory-reset a field kit " +
			"were readable by anyone who saw the SMS")
	}
}

// The per-hour ceiling is per TENANT as well as per bridge and bearer. A bridge
// id is only unique within a tenant -- store.OOBPeer is keyed by both -- so with
// a key of bridge|bearer alone, two tenants that named a bridge the same way
// shared one budget and either could exhaust the other's ability to command its
// own hardware (MESHSAT-1121).
func TestTheRateCeilingIsPerTenantNotJustPerBridge(t *testing.T) {
	svc, _, _ := newService(t) // MaxPerHour: 3
	const bridge, bearer = "tesseract", BearerSMS

	for i := 0; i < 3; i++ {
		if !svc.allow("t_one", bridge, bearer, 3) {
			t.Fatalf("tenant one was blocked on send %d of its own budget", i+1)
		}
	}
	if svc.allow("t_one", bridge, bearer, 3) {
		t.Fatal("tenant one exceeded its own ceiling")
	}
	if !svc.allow("t_two", bridge, bearer, 3) {
		t.Fatal("a second tenant with a bridge of the same name was refused because " +
			"the FIRST tenant had used its budget: one customer can stop another " +
			"commanding their own field kit")
	}
}

// A tenant's own ceiling is used, not the platform's. The platform number
// remains the default for a tenant that has not chosen one.
func TestATenantsOwnCeilingApplies(t *testing.T) {
	svc, _, _ := newService(t) // platform default MaxPerHour: 3

	svc.SetPolicy(func(_ context.Context, tenantID string) Policy {
		if tenantID == "t_generous" {
			return Policy{MaxPerHour: 5}
		}
		return Policy{} // unset: fall back to the platform's
	})

	if got := svc.policyFor(context.Background(), "t_generous").MaxPerHour; got != 5 {
		t.Errorf("the tenant's own ceiling was %d, want 5", got)
	}
	if got := svc.policyFor(context.Background(), "t_plain").MaxPerHour; got != 3 {
		t.Errorf("a tenant that chose nothing got %d, want the platform default 3", got)
	}
}
