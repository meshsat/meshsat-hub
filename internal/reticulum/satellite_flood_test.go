package reticulum

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// MESHSAT-1296: an announce flooded onto the Iridium interface became a
// Cloudloop call with no thing, every ten minutes on each replica, refused each
// time and logged as sent. A satellite MT goes to one modem; flooding skips the
// satellite interfaces, and one asked to send to nobody refuses before any call.
func TestAnnouncesAreNotFloodedOntoSatellite(t *testing.T) {
	var calls atomic.Int32
	send := func(context.Context, string, []byte) error { calls.Add(1); return nil }
	avail := func(context.Context) bool { return true }

	iridium := NewIridiumInterface(NewBackendAdapter(send, avail, 270, 0.1))
	iridium.SetAvailable(true)
	globalstar := NewGlobalstarInterface(NewBackendAdapter(send, avail, 128, 0.1))
	globalstar.SetAvailable(true)
	tcp := &mockInterface{name: IfaceMQTT, mtu: 500, available: true}

	relay := NewRelay(NewRouter(0), DefaultRelayConfig())
	relay.RegisterInterface(iridium)
	relay.RegisterInterface(globalstar)
	relay.RegisterInterface(tcp)

	relay.Broadcast(context.Background(), IfaceTCP, make([]byte, 60))
	if tcp.sent != 1 {
		t.Fatalf("the terrestrial interface got %d copies, want 1", tcp.sent)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("a flood reached a satellite backend %d times", n)
	}

	for _, iface := range []Interface{iridium, globalstar} {
		if err := iface.Send(context.Background(), "", make([]byte, 10)); !errors.Is(err, ErrNoDestination) {
			t.Fatalf("%s: send to nobody: %v", iface.Name(), err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("a send with no destination reached a satellite backend %d times", n)
	}
	if err := iridium.Send(context.Background(), "300000000000001", make([]byte, 10)); err != nil || calls.Load() != 1 {
		t.Fatalf("an addressed send did not go through: %v (calls %d)", err, calls.Load())
	}
}
