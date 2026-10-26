package takhosted

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// TestEndToEndDirectChatsOnARealOpenTAKServer proves against a real
// OpenTAKServer what the dispatcher-to-crew chat rests on (MESHSAT-1706):
//
//   - a chat addressed to one contact reaches the connection that announced that
//     contact, by callsign (ATAK, WinTAK) and by uid (iTAK);
//   - a chat addressed to a crew the Hub only FORWARDS on its own connection is
//     not delivered to the Hub on a stock server (the gap), while a chat to All
//     Chat Rooms is (the control);
//   - a reply the Hub writes for a crew, addressed to a dispatcher, reaches that
//     dispatcher although the connection's own identity is the Hub's;
//   - with TAKHOSTED_E2E_PATCHED=1, the relay event (patch-relay-dms.py) makes
//     both dest forms reach the Hub, an unknown callsign still does not, and a
//     Hub connection that has not sent it gets nothing.
//
// It is skipped unless a server is pointed at, because CI has none. Run it
// against a throwaway server, never a tenant's (a probe there needs the owner's
// go, .claude/rules/tak.md):
//
//	kubectl -n meshsat-tak-spike port-forward deploy/ots-spike 18089:8089 &
//	TAKHOSTED_E2E_UPSTREAM=127.0.0.1:18089 TAKHOSTED_E2E_CA=ca.pem \
//	TAKHOSTED_E2E_HUB_CERT=meshsat_hub.pem TAKHOSTED_E2E_HUB_KEY=meshsat_hub.key \
//	TAKHOSTED_E2E_USER_CERT=spikeuser.pem TAKHOSTED_E2E_USER_KEY=spikeuser.key \
//	go test ./internal/takhosted/ -run EndToEndDirectChats -v
func TestEndToEndDirectChatsOnARealOpenTAKServer(t *testing.T) {
	addr := os.Getenv("TAKHOSTED_E2E_UPSTREAM")
	if addr == "" {
		t.Skip("set TAKHOSTED_E2E_UPSTREAM to run against a real OpenTAKServer")
	}
	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(spikeEnv(t, "TAKHOSTED_E2E_CA"))
	if err != nil || !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("read the server's CA: %v", err)
	}
	hubID := spikeIdentity(t, "TAKHOSTED_E2E_HUB_CERT", "TAKHOSTED_E2E_HUB_KEY")
	userID := spikeIdentity(t, "TAKHOSTED_E2E_USER_CERT", "TAKHOSTED_E2E_USER_KEY")
	dial := func(id tls.Certificate) *spikeConn {
		conn, err := takfront.DialUpstream(context.Background(), &takfront.Tenant{
			TenantID: "spike", Upstream: addr, Identity: id, UpstreamCAs: pool,
		}, 15*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		c := &spikeConn{conn: conn}
		go c.readLoop()
		t.Cleanup(func() { _ = conn.Close() })
		return c
	}

	run := spikeHex(t)
	x := spikeContact{uid: "spike-x-" + run, callsign: "SPIKE-X-" + run}
	y := spikeContact{uid: "spike-y-" + run, callsign: "SPIKE-Y-" + run}
	crew := spikeContact{uid: "meshsat-c0de" + run[:4], callsign: "SPIKE-C-" + run}

	cx, cy := dial(userID), dial(userID)
	cx.write(t, spikePLI(x))
	cy.write(t, spikePLI(y))
	// Both are identified once each has seen the other's position.
	cx.await(t, "x sees y", y.uid, func() { cy.write(t, spikePLI(y)) })
	cy.await(t, "y sees x", x.uid, func() { cx.write(t, spikePLI(x)) })

	t.Run("dm by callsign reaches the named contact", func(t *testing.T) {
		m := "dm-callsign-" + spikeHex(t)
		cy.await(t, m, m, func() { cx.write(t, spikeChat(x, y, m, destCallsign)) })
	})
	t.Run("dm by uid reaches the named contact", func(t *testing.T) {
		m := "dm-uid-" + spikeHex(t)
		cy.await(t, m, m, func() { cx.write(t, spikeChat(x, y, m, destUID)) })
	})

	// The Hub: its hello first, then a crew position forwarded for a bridge,
	// exactly as the shared link does.
	ch := dial(hubID)
	ch.write(t, hubHello("spike", time.Now()))
	ch.write(t, spikePLI(crew))
	ch.await(t, "hub sees x", x.uid, func() { cx.write(t, spikePLI(x)) })

	t.Run("all chat rooms reaches the hub (control)", func(t *testing.T) {
		m := "room-" + spikeHex(t)
		ch.await(t, m, m, func() { cx.write(t, spikeChat(x, spikeContact{}, m, destNone)) })
	})

	if os.Getenv("TAKHOSTED_E2E_PATCHED") == "" {
		t.Run("a dm to a forwarded crew does not reach the hub on a stock server", func(t *testing.T) {
			byCallsign, byUID := "gap-cs-"+spikeHex(t), "gap-uid-"+spikeHex(t)
			cx.write(t, spikeChat(x, crew, byCallsign, destCallsign))
			cx.write(t, spikeChat(x, crew, byUID, destUID))
			ch.absent(t, 8*time.Second, byCallsign, byUID)
		})
	} else {
		ch.write(t, spikeRelay(crew))
		t.Run("after the relay event a dm by callsign reaches the hub", func(t *testing.T) {
			m := "relay-cs-" + spikeHex(t)
			ch.await(t, m, m, func() { cx.write(t, spikeChat(x, crew, m, destCallsign)) })
		})
		t.Run("after the relay event a dm by uid reaches the hub", func(t *testing.T) {
			m := "relay-uid-" + spikeHex(t)
			ch.await(t, m, m, func() { cx.write(t, spikeChat(x, crew, m, destUID)) })
		})
		t.Run("a dm to a callsign nobody relays does not reach the hub", func(t *testing.T) {
			m := "nobody-" + spikeHex(t)
			cx.write(t, spikeChat(x, spikeContact{uid: "spike-nobody", callsign: "SPIKE-NOBODY"}, m, destCallsign))
			ch.absent(t, 6*time.Second, m)
		})
		t.Run("the relay event is never published to a client", func(t *testing.T) {
			cx.absent(t, 2*time.Second, "t-x-meshsat-relay")
			cy.absent(t, 0, "t-x-meshsat-relay")
		})
		t.Run("a relay event from anybody but the hub binds nothing", func(t *testing.T) {
			stolen := spikeContact{uid: "meshsat-5701" + run[:4], callsign: "SPIKE-S-" + run}
			ch.write(t, spikeRelay(crew, stolen))
			cy.write(t, spikeRelay(stolen))
			m := "stolen-" + spikeHex(t)
			// The Hub holds the binding (positive control); y, which tried to
			// claim the same contact, must not get it.
			ch.await(t, m, m, func() { cx.write(t, spikeChat(x, stolen, m, destCallsign)) })
			cy.absent(t, 3*time.Second, m)
			cx.absent(t, 0, "t-x-meshsat-relay")
		})
		t.Run("a hub connection that never relayed gets nothing", func(t *testing.T) {
			other := dial(hubID)
			other.write(t, hubHello("spike-other", time.Now()))
			other.await(t, "second hub sees x", x.uid, func() { cx.write(t, spikePLI(x)) })
			m := "unrelayed-" + spikeHex(t)
			// The first Hub connection still holds the binding: it is the
			// positive control that the chat was routed at all.
			ch.await(t, m, m, func() { cx.write(t, spikeChat(x, crew, m, destCallsign)) })
			other.absent(t, 3*time.Second, m)
		})
	}

	t.Run("a reply the hub writes for a crew reaches the dispatcher", func(t *testing.T) {
		m := "reply-" + spikeHex(t)
		cx.await(t, m, m, func() { ch.write(t, spikeChat(crew, x, m, destCallsign)) })
	})
}

type spikeContact struct{ uid, callsign string }

type spikeConn struct {
	conn net.Conn
	mu   sync.Mutex
	got  []byte
}

// readLoop is a plain conn.Read in a loop, unblocked by the connection being
// closed (rule 15).
func (c *spikeConn) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(buf)
		c.mu.Lock()
		c.got = append(c.got, buf[:n]...)
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *spikeConn) has(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Contains(c.got, []byte(s))
}

func (c *spikeConn) write(t *testing.T, b []byte) {
	t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// await resends with again until s has been read, rather than sleeping a
// guessed interval: the server needs a moment to declare a new connection's
// queues.
func (c *spikeConn) await(t *testing.T, what, s string, again func()) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		again()
		for i := 0; i < 20; i++ {
			if c.has(s) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("%s: never read %q; %d bytes read: %.600q", what, s, len(c.got), c.got)
}

// absent fails if any of ss is read within wait.
func (c *spikeConn) absent(t *testing.T, wait time.Duration, ss ...string) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		for _, s := range ss {
			if c.has(s) {
				t.Fatalf("read %q, which must not have been delivered here", s)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const spikeTime = "2006-01-02T15:04:05.000Z"

func spikeTimes(life time.Duration) (string, string) {
	now := time.Now().UTC()
	return now.Format(spikeTime), now.Add(life).Format(spikeTime)
}

// spikePLI is a crew or dispatcher position, as ATAK sends it.
func spikePLI(c spikeContact) []byte {
	now, stale := spikeTimes(30 * time.Second)
	return []byte(fmt.Sprintf(`<event version="2.0" uid="%s" type="a-f-G-U-C" how="m-g" time="%s" start="%s" stale="%s">`+
		`<point lat="53.0" lon="3.0" hae="0" ce="10" le="10"/>`+
		`<detail><contact callsign="%s" endpoint="*:-1:stcp"/><__group name="Cyan" role="Team Member"/></detail></event>`+"\n",
		c.uid, now, now, stale, c.callsign))
}

type spikeDest int

const (
	destNone spikeDest = iota
	destCallsign
	destUID
)

// spikeChat is a GeoChat in ATAK's shape; an empty to is All Chat Rooms.
func spikeChat(from, to spikeContact, text string, dest spikeDest) []byte {
	now, stale := spikeTimes(time.Minute)
	room, roomID := "All Chat Rooms", "All Chat Rooms"
	if to.uid != "" {
		room, roomID = to.callsign, to.uid
	}
	marti := ""
	switch dest {
	case destCallsign:
		marti = fmt.Sprintf(`<marti><dest callsign="%s"/></marti>`, to.callsign)
	case destUID:
		marti = fmt.Sprintf(`<marti><dest uid="%s"/></marti>`, to.uid)
	}
	return []byte(fmt.Sprintf(`<event version="2.0" uid="GeoChat.%s.%s.%s" type="b-t-f" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="0.0" lon="0.0" hae="9999999.0" ce="9999999.0" le="9999999.0"/>`+
		`<detail><__chat parent="RootContactGroup" groupOwner="false" messageId="%s" chatroom="%s" id="%s" senderCallsign="%s">`+
		`<chatgrp uid0="%s" uid1="%s" id="%s"/></__chat>`+
		`<link uid="%s" type="a-f-G-U-C" relation="p-p"/>`+
		`<remarks source="BAO.F.ATAK.%s" to="%s" time="%s">%s</remarks>%s</detail></event>`+"\n",
		from.uid, roomID, text, now, now, stale,
		text, room, roomID, from.callsign,
		from.uid, roomID, roomID,
		from.uid,
		from.uid, roomID, now, text, marti))
}

// spikeRelay is the Hub-only event patch-relay-dms.py acts on.
func spikeRelay(cs ...spikeContact) []byte {
	now, stale := spikeTimes(time.Minute)
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, `<relay uid="%s" callsign="%s"/>`, c.uid, c.callsign)
	}
	return []byte(fmt.Sprintf(`<event version="2.0" uid="%srelay" type="t-x-meshsat-relay" how="h-g-i-g-o" time="%s" start="%s" stale="%s">`+
		`<point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail>%s</detail></event>`+"\n",
		hubUIDPrefix, now, now, stale, b.String()))
}

func spikeEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

func spikeIdentity(t *testing.T, certVar, keyVar string) tls.Certificate {
	t.Helper()
	id, err := tls.LoadX509KeyPair(spikeEnv(t, certVar), spikeEnv(t, keyVar))
	if err != nil {
		t.Fatalf("load %s: %v", certVar, err)
	}
	return id
}

func spikeHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
