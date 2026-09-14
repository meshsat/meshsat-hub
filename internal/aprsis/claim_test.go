package aprsis

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// MESHSAT-1120. Both Hub replicas subscribe to every topic with a plain
// Subscribe, and shouldSend is a per-REPLICA map, so both would inject the same
// packet into APRS-IS -- a public network, under our own callsign.
//
// This is latent rather than live: HUB_APRSIS_ENABLED is false in production.
// It is fixed now because the moment it is switched on is the moment it would
// be easiest to forget.

type fakeClaimer struct {
	mu     sync.Mutex
	claims map[string]bool
	err    error
	calls  int
}

func newFakeClaimer() *fakeClaimer { return &fakeClaimer{claims: map[string]bool{}} }

func (f *fakeClaimer) ClaimOnce(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}

func TestOnlyOneReplicaTransmits(t *testing.T) {
	shared := newFakeClaimer()
	const topic = "meshsat/dev-1/position"
	payload := []byte(`{"lat":52.37,"lon":4.9,"source":"iridium"}`)

	won := 0
	for i := 0; i < 2; i++ { // two replicas, one shared claim table
		s := NewSubscriber(nil, nil, nil, 60)
		s.SetClaimer(shared)
		if s.claim(topic, payload) {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d replicas transmitted the same packet, want 1. APRS-IS is somebody "+
			"else's network and the callsign is ours.", won)
	}
}

// A DIFFERENT message still goes out -- the claim identifies the message.
func TestADifferentPacketIsStillTransmitted(t *testing.T) {
	shared := newFakeClaimer()
	s := NewSubscriber(nil, nil, nil, 60)
	s.SetClaimer(shared)

	if !s.claim("meshsat/dev-1/position", []byte(`{"lat":1,"lon":2}`)) {
		t.Fatal("the first packet was not claimed")
	}
	if !s.claim("meshsat/dev-1/position", []byte(`{"lat":3,"lon":4}`)) {
		t.Error("a distinct packet was suppressed; the claim is not keyed on the message")
	}
}

// Two devices at identical coordinates are two packets.
func TestTwoDevicesAreTwoPackets(t *testing.T) {
	shared := newFakeClaimer()
	s := NewSubscriber(nil, nil, nil, 60)
	s.SetClaimer(shared)
	payload := []byte(`{"lat":52.37,"lon":4.9,"source":"iridium"}`)

	if !s.claim("meshsat/dev-a/position", payload) || !s.claim("meshsat/dev-b/position", payload) {
		t.Error("two devices at the same coordinates collided into one packet")
	}
}

// It fails CLOSED, which is the OPPOSITE of webhook delivery and deliberate: a
// duplicate packet on a public network under our callsign is worse than a
// missed one on a best-effort feed.
func TestAFailedClaimDoesNotTransmit(t *testing.T) {
	c := newFakeClaimer()
	c.err = errors.New("database is down")
	s := NewSubscriber(nil, nil, nil, 60)
	s.SetClaimer(c)

	if s.claim("meshsat/dev-1/position", []byte(`{}`)) {
		t.Error("transmitted despite a failed claim; this must fail closed, unlike " +
			"webhook delivery which fails open")
	}
}

// With no claimer -- a single-replica or unconfigured Hub -- behaviour is
// exactly what it was before this existed.
func TestWithNoClaimerEverythingTransmits(t *testing.T) {
	s := NewSubscriber(nil, nil, nil, 60)
	if !s.claim("meshsat/dev-1/position", []byte(`{}`)) {
		t.Error("an unconfigured Hub stopped transmitting")
	}
}

// allowAll is a PlatformChecker that lets the handler run.
type allowAll struct{}

func (allowAll) IsPlatformTopic(context.Context, string) bool { return true }

// The claim being correct is worth nothing if the HANDLER does not consult it.
// These call handlePosition itself rather than s.claim, so removing the call
// site is caught rather than only removing the logic.
func TestThePositionHandlerConsultsTheClaim(t *testing.T) {
	shared := newFakeClaimer()
	const topic = "meshsat/dev-1/position"
	// source must be a satellite one or the handler discards it before this.
	payload := []byte(`{"lat":52.37,"lon":4.9,"source":"iridium"}`)

	for i := 0; i < 2; i++ { // two replicas
		s := NewSubscriber(nil, NewClient("", "MESHSAT", 9, "", ""), allowAll{}, 60)
		s.SetClaimer(shared)
		s.handlePosition(topic, payload)
	}

	if shared.calls != 2 {
		t.Fatalf("the claim was consulted %d times for two replicas, want 2. The handler "+
			"is transmitting without asking, so both replicas inject the same packet.",
			shared.calls)
	}
	if n := len(shared.claims); n != 1 {
		t.Errorf("%d distinct claims for one message, want 1", n)
	}
}

func TestTheMOHandlerConsultsTheClaim(t *testing.T) {
	shared := newFakeClaimer()
	const topic = "meshsat/dev-1/mo/decoded"
	payload := []byte(`{"imei":"dev-1","text":"hello","iridium_latitude":52.37,"iridium_longitude":4.9}`)

	for i := 0; i < 2; i++ {
		s := NewSubscriber(nil, NewClient("", "MESHSAT", 9, "", ""), allowAll{}, 60)
		s.SetClaimer(shared)
		s.handleMODecoded(topic, payload)
	}

	if shared.calls != 2 {
		t.Errorf("the MO handler consulted the claim %d times, want 2", shared.calls)
	}
}
