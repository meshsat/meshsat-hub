package rnsnode

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

type frameSink struct {
	mu     sync.Mutex
	frames map[string][][]byte
}

func (f *frameSink) take(id string, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames[id] = append(f.frames[id], append([]byte(nil), raw...))
}

func (f *frameSink) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames[id])
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func readFrame(t *testing.T, c net.Conn) []byte {
	t.Helper()
	r := rnswire.NewHDLCFrameReader()
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if frames := r.Feed(buf[:n]); len(frames) > 0 {
			return frames[0]
		}
	}
}

func packet(fill byte) []byte {
	p := make([]byte, rnswire.HeaderMinSize+8)
	for i := range p {
		p[i] = fill
	}
	return p
}

// Two clients are two interfaces: a frame from one arrives under its own id,
// Send reaches only the named one, Broadcast reaches both.
func TestTCPServer_EachConnectionIsAnInterface(t *testing.T) {
	sink := &frameSink{frames: map[string][][]byte{}}
	srv := NewTCPServer("tcpb", "127.0.0.1:0", sink.take)
	var changes []string
	var cmu sync.Mutex
	srv.OnChange(func(id string, up bool) {
		cmu.Lock()
		defer cmu.Unlock()
		changes = append(changes, id)
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	a, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	waitFor(t, "two connections", func() bool { return srv.Count() == 2 })
	ifs := srv.Interfaces()
	if len(ifs) != 2 || ifs[0] != "tcpb_1" || ifs[1] != "tcpb_2" {
		t.Fatalf("interfaces %v", ifs)
	}

	if _, err := a.Write(rnswire.HDLCFrame(packet(0xA1))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "frame from a", func() bool { return sink.count("tcpb_1") == 1 })
	if sink.count("tcpb_2") != 0 || !bytes.Equal(sink.frames["tcpb_1"][0], packet(0xA1)) {
		t.Fatalf("frame attributed wrongly: %+v", sink.frames)
	}

	if err := srv.Send("tcpb_2", packet(0xB2)); err != nil {
		t.Fatal(err)
	}
	if got := readFrame(t, b); !bytes.Equal(got, packet(0xB2)) {
		t.Fatalf("b got %x", got)
	}
	if err := srv.Send("tcpb_9", packet(0x00)); err != ErrNoSuchInterface {
		t.Fatalf("send to unknown: %v", err)
	}
	if n := srv.Broadcast(packet(0xC3)); n != 2 {
		t.Fatalf("broadcast reached %d", n)
	}
	for _, c := range []net.Conn{a, b} {
		if got := readFrame(t, c); !bytes.Equal(got, packet(0xC3)) {
			t.Fatalf("broadcast frame %x", got)
		}
	}

	_ = a.Close()
	waitFor(t, "a gone", func() bool { return srv.Count() == 1 })
	if !srv.Owns("tcpb_1") || srv.Owns("tcpp_1") {
		t.Fatal("Owns is by listener name")
	}
	cmu.Lock()
	defer cmu.Unlock()
	if len(changes) != 3 || changes[0] != "tcpb_1" || changes[1] != "tcpb_2" || changes[2] != "tcpb_1" {
		t.Fatalf("changes %v", changes)
	}
	if st := srv.Stats(); st.Accepted != 2 || st.RxFrames != 1 || st.TxFrames != 3 {
		t.Fatalf("stats %+v", st)
	}
}

// Beyond MaxConns a connection is closed at once and counted; a departed
// peer frees its slot.
func TestTCPServer_RefusesBeyondMaxConns(t *testing.T) {
	sink := &frameSink{frames: map[string][][]byte{}}
	srv := NewTCPServer("tcpp", "127.0.0.1:0", sink.take)
	srv.SetMaxConns(1)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	a, _ := net.Dial("tcp", srv.Addr())
	defer a.Close()
	waitFor(t, "first", func() bool { return srv.Count() == 1 })
	b, _ := net.Dial("tcp", srv.Addr())
	defer b.Close()
	waitFor(t, "refused", func() bool { return srv.Stats().Refused == 1 })
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("refused connection stayed open")
	}
	_ = a.Close()
	waitFor(t, "slot freed", func() bool { return srv.Count() == 0 })
	c, _ := net.Dial("tcp", srv.Addr())
	defer c.Close()
	waitFor(t, "third accepted", func() bool { return srv.Count() == 1 && srv.Stats().Accepted == 2 })
}
