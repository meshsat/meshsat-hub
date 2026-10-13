package rns

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/msgpack"
)

// A request on a link is answered by the destination's handler: a small one
// as a RESPONSE packet, a large one as a resource; an unknown path and a
// handler that declines get no response and the receipt times out; the
// handler sees the identity the peer identified with.
func TestLinkRequestsAndResponses(t *testing.T) {
	net := &fakeNet{wires: map[string]*wire{}}
	a, _ := newTestNode(t, net, "a")
	b, _ := newTestNode(t, net, "b")
	connect(net, a, "a", "ab", b, "b", "ba")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	b.Start(ctx)

	var seenIdentity [HashLen]byte
	dest := b.AddDestination(&Destination{Name: "test.req", AcceptLinks: true})
	dest.RegisterRequestHandler("/echo", func(req *Request) (msgpack.Value, bool) {
		if req.RemoteIdentity != nil {
			seenIdentity = req.RemoteIdentity.IdentityHash()
		}
		return msgpack.Array(msgpack.Str("echo"), req.Data), true
	})
	dest.RegisterRequestHandler("/big", func(req *Request) (msgpack.Value, bool) {
		return msgpack.Bin(bytes.Repeat([]byte{0x42}, 3000)), true
	})
	dest.RegisterRequestHandler("/silent", func(req *Request) (msgpack.Value, bool) { return msgpack.Nil(), false })
	if err := b.Announce(dest, "", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a learns b", func() bool { return a.Paths().Has(dest.Hash) })
	l, err := a.Links().Initiate(dest.Hash)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "link active", func() bool { return l.State() == LinkActive })

	// Small request, packet response.
	rec, err := a.Links().Request(l, "/echo", msgpack.Str("hello"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-rec.Done():
		if res.Err != nil || res.Response.Kind != msgpack.KindArray || len(res.Response.Array) != 2 || res.Response.Array[1].Str != "hello" {
			t.Fatalf("echo response %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo response")
	}
	if seenIdentity != ([HashLen]byte{}) {
		t.Fatal("handler saw an identity before the peer identified")
	}

	// Identify, then the handler sees who asked.
	if err := a.Links().Identify(l, a.Identity()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "identified", func() bool {
		return l.RemoteIdentity() != nil || b.Links().Get(l.ID) != nil && b.Links().Get(l.ID).RemoteIdentity() != nil
	})
	rec, _ = a.Links().Request(l, "/echo", msgpack.Int(7), 5*time.Second)
	res := <-rec.Done()
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if seenIdentity != a.IdentityHash() {
		t.Fatalf("handler saw identity %x, want %x", seenIdentity, a.IdentityHash())
	}

	// Large request data and a large response both travel as resources.
	rec, err = a.Links().Request(l, "/big", msgpack.Bin(bytes.Repeat([]byte{1}, 2000)), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-rec.Done():
		if res.Err != nil || len(res.Response.Bin) != 3000 || res.Response.Bin[0] != 0x42 {
			t.Fatalf("big response %+v", res.Err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no big response")
	}

	// Unknown path and a declining handler: no response, the receipt times out.
	for _, path := range []string{"/nope", "/silent"} {
		rec, err := a.Links().Request(l, path, msgpack.Nil(), 500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case res := <-rec.Done():
			if res.Err != ErrRequestTimeout {
				t.Fatalf("%s: %+v", path, res)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: receipt never resolved", path)
		}
	}

	// A closing link ends what is still pending.
	rec, _ = a.Links().Request(l, "/silent", msgpack.Nil(), time.Minute)
	a.Links().Teardown(l)
	select {
	case res := <-rec.Done():
		if res.Err == nil {
			t.Fatal("pending request survived the teardown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not end the pending request")
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}
