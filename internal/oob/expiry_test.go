package oob

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// MESHSAT-1293: a request carries the time after which the bridge must not act
// on it, and the Hub notices a reply to a command it had given up on.

func seqKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// The bytes the bridge's codec (meshsat cd1267f) seals for the same inputs,
// produced by compiling that frame.go on its own. If these move, a kit and the
// Hub no longer agree on the wire.
func TestExpiryFrameMatchesTheBridgesBytes(t *testing.T) {
	want := map[bool]string{
		false: "4f18123400000007016ab0f2100a978a5736893d647697c3c0bdba2897c8",
		true:  "4f191234000000070100bc26851f205d82d66d8996e285dca288f38ede06",
	}
	for enc, hexWire := range want {
		f := Frame{Enc: enc, PeerID: 0x1234, Counter: 7, Cmd: CmdPing, Args: []byte{0x0A}, ExpiresAt: 1789981200}
		w, err := Seal(f, seqKey(), RoleImporter)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(w); got != hexWire {
			t.Fatalf("enc=%v: sealed %s, the bridge seals %s", enc, got, hexWire)
		}
		// And the bridge's bytes open here, with the expiry split off the args.
		raw, _ := hex.DecodeString(hexWire)
		back, err := Open(raw, seqKey(), RoleImporter)
		if err != nil {
			t.Fatalf("enc=%v: the bridge's frame does not open: %v", enc, err)
		}
		if back.ExpiresAt != 1789981200 || !bytes.Equal(back.Args, []byte{0x0A}) {
			t.Fatalf("enc=%v: opened %+v", enc, back)
		}
	}
}

func TestExpiryIsAuthenticatedAndCountsAgainstTheArgs(t *testing.T) {
	key := seqKey()
	// 69 bytes of args plus the 4-byte expiry is the most one frame can carry.
	if _, err := Seal(Frame{PeerID: 1, Counter: 1, Cmd: CmdPing, Args: make([]byte, MaxArgs-ExpiryLen), ExpiresAt: 1}, key, RoleIssuer); err != nil {
		t.Fatalf("69 bytes + expiry refused: %v", err)
	}
	if _, err := Seal(Frame{PeerID: 1, Counter: 1, Cmd: CmdPing, Args: make([]byte, MaxArgs-ExpiryLen+1), ExpiresAt: 1}, key, RoleIssuer); !errors.Is(err, ErrArgsLen) {
		t.Fatalf("70 bytes + expiry accepted: %v", err)
	}
	// A frame without an expiry is exactly the v1.1 frame, flag clear.
	w, _ := Seal(Frame{PeerID: 1, Counter: 1, Cmd: CmdPing}, key, RoleIssuer)
	if w[1]&FlagExpiry != 0 {
		t.Fatalf("expiry flag set on a frame without one")
	}
	// Changing the expiry in a clear frame breaks the tag: it cannot be extended.
	w, _ = Seal(Frame{PeerID: 1, Counter: 1, Cmd: CmdPing, ExpiresAt: 1000}, key, RoleIssuer)
	binary.BigEndian.PutUint32(w[HeaderLen:], 2000)
	if _, err := Open(w, key, RoleIssuer); !errors.Is(err, ErrAuth) {
		t.Fatalf("a rewritten expiry opened: %v", err)
	}
}

// A frame whose flag promises an expiry it does not carry is malformed, even
// with a valid tag.
func TestAFlagWithoutAnExpiryIsRefused(t *testing.T) {
	key := seqKey()
	for name, body := range map[string][]byte{"short": {0x01, 0x02}, "zero": {0, 0, 0, 0}} {
		gcm, _ := newGCM(key)
		hdr := []byte{Magic, Version<<4 | FlagExpiry, 0, 1, 0, 0, 0, 1, CmdPing}
		nonce := Nonce(1, RoleIssuer, DirRequest, 1)
		aad := append(append([]byte{}, hdr...), body...)
		tag := gcm.Seal(nil, nonce[:], nil, aad)
		wire := append(append(append([]byte{}, hdr...), body...), tag...)
		if _, err := Open(wire, key, RoleIssuer); !errors.Is(err, ErrBadExpiry) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The request expires when the Hub stops waiting for it, so a bridge with a
// set clock never runs a command whose caller was already told it failed.
func TestARequestExpiresWhenTheHubStopsWaiting(t *testing.T) {
	svc, tr, key := newService(t)
	tr.timeout = 300 * time.Millisecond
	ctx := context.Background()
	if _, err := svc.Pair(ctx, "t1", "kit-a", key, RoleImporter, "+31600000001", ""); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	_, err := svc.Send(ctx, "t1", "kit-a", BearerSMS, "mgmt_ping", ArgSpec{}, false)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "may still reach the bridge") {
		t.Fatalf("the timeout does not say the command may still arrive: %v", err)
	}
	wire, _ := Decode(waitForSend(t, tr))
	req, err := Open(wire, key, RoleImporter)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Unix(int64(req.ExpiresAt), 0)
	if exp.Before(before.Add(tr.timeout)) || exp.After(before.Add(tr.timeout+2*time.Second)) {
		t.Fatalf("expires %s, sent %s with a %s wait", exp, before, tr.timeout)
	}
}

type memLate struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (l *memLate) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[k] = v
	return nil
}

func (l *memLate) Get(_ context.Context, k string) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.m[k]
	return v, ok, nil
}

// A reply that arrives after the Hub gave up is reported as late, with its
// age; a reply the Hub was still waiting for is an ordinary one.
func TestAReplyAfterTheHubGaveUpIsReportedLate(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc, tr, key := newService(t)
	svc.SetLateStore(&memLate{m: map[string][]byte{}})
	tr.timeout = 200 * time.Millisecond
	ctx := context.Background()
	if _, err := svc.Pair(ctx, "t1", "kit-a", key, RoleImporter, "+31600000001", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, "t1", "kit-a", BearerSMS, "mgmt_ping", ArgSpec{}, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	wire, _ := Decode(waitForSend(t, tr))
	req, _ := Open(wire, key, RoleImporter)

	reply := Frame{Enc: true, Reply: true, PeerID: req.PeerID, Counter: 5, Cmd: req.Cmd,
		Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: uint16(req.Counter), Seq: 1, Total: 1, Body: []byte("u1h")})}
	rw, _ := Seal(reply, key, RoleIssuer)
	if !svc.HandleInbound(ctx, BearerSMS, "+31600000001", Encode(rw)) {
		t.Fatal("reply not classified")
	}
	if !strings.Contains(buf.String(), "LATE reply") || !strings.Contains(buf.String(), "cmd=PING") {
		t.Fatalf("a reply after the timeout was not reported late:\n%s", buf.String())
	}

	// A reply the Hub is still waiting for is not late.
	buf.Reset()
	tr.timeout = 5 * time.Second
	tr.mu.Lock()
	tr.sent = nil
	tr.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := svc.Send(ctx, "t1", "kit-a", BearerSMS, "mgmt_ping", ArgSpec{}, false)
		done <- err
	}()
	wire, _ = Decode(waitForSend(t, tr))
	req, _ = Open(wire, key, RoleImporter)
	reply = Frame{Enc: true, Reply: true, PeerID: req.PeerID, Counter: 6, Cmd: req.Cmd,
		Args: EncodeReplyArgs(ReplyArgs{RC: RCOK, ReqCounterLo: uint16(req.Counter), Seq: 1, Total: 1, Body: []byte("u1h")})}
	rw, _ = Seal(reply, key, RoleIssuer)
	svc.HandleInbound(ctx, BearerSMS, "+31600000001", Encode(rw))
	if err := <-done; err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(buf.String(), "LATE reply") {
		t.Fatalf("a reply in time was reported late:\n%s", buf.String())
	}
}
