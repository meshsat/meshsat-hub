package bridge

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeMember reads one MQTT CONNECT and answers CONNACK: accepted when the
// password matches want.
func fakeMember(t *testing.T, want string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 0x10 {
					return
				}
				body := make([]byte, hdr[1]) // short test packets: one-byte length
				if _, err := io.ReadFull(c, body); err != nil {
					return
				}
				// protocol name, level, flags, keep-alive, then client id, user, pass
				off := 2 + 4 + 1 + 1 + 2
				var fields []string
				for i := 0; i < 3; i++ {
					n := int(binary.BigEndian.Uint16(body[off:]))
					fields = append(fields, string(body[off+2:off+2+n]))
					off += 2 + n
				}
				rc := byte(5)
				if fields[2] == want && strings.HasPrefix(fields[0], "hub-credprobe-") {
					rc = 0
				}
				_, _ = c.Write([]byte{0x20, 0x02, 0x00, rc})
				_, _ = io.ReadAll(c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// The prober answers what a scanned phone will meet: live only when EVERY
// member takes the credential, and it never logs in under the bridge's own
// client id (that would take the bridge's live session over).
func TestTheProberNeedsEveryMember(t *testing.T) {
	members := map[string]string{
		"m1": fakeMember(t, "new-pass"),
		"m2": fakeMember(t, "old-pass"), // this member has not reloaded yet
		"m3": fakeMember(t, "new-pass"),
	}
	p := NewCredentialProber("nats-headless.test:1883")
	p.Timeout = 2 * time.Second
	p.lookup = func(context.Context, string) ([]string, error) { return []string{"m3", "m1", "m2"}, nil }
	d := &net.Dialer{}
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		return d.DialContext(ctx, network, members[host])
	}
	live, res, err := p.Live(context.Background(), "kit-a", "new-pass")
	if err != nil || live || len(res) != 3 {
		t.Fatalf("one member behind must not be live: live=%v res=%+v err=%v", live, res, err)
	}
	if !res[0].Accepted || res[1].Accepted || !res[2].Accepted {
		t.Fatalf("per member (sorted m1,m2,m3): %+v", res)
	}

	// No cache flush here, on purpose: a "not yet" is never cached, so the moment
	// the lagging member reloads, the very next check says live.
	members["m2"] = fakeMember(t, "new-pass")
	if live, res, _ := p.Live(context.Background(), "kit-a", "new-pass"); !live {
		t.Fatalf("all members accept, still not live: %+v", res)
	}
	if live, _, _ := p.Live(context.Background(), "kit-a", "wrong"); live {
		t.Fatal("a wrong password is live")
	}
}
