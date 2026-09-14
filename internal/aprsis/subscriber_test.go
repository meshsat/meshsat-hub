package aprsis

import (
	"testing"
	"time"
)

func newCoalescer(sec int) *Subscriber {
	return &Subscriber{coalesceSec: sec, lastSent: make(map[string]time.Time)}
}

func TestShouldSend_FirstTime(t *testing.T) {
	s := newCoalescer(60)
	if !s.shouldSend("default", "device-1") {
		t.Error("first send should be allowed")
	}
}

func TestShouldSend_RateLimited(t *testing.T) {
	s := newCoalescer(60)
	s.shouldSend("default", "device-1") // first send
	if s.shouldSend("default", "device-1") {
		t.Error("second send within coalesce window should be blocked")
	}
}

func TestShouldSend_DifferentDevices(t *testing.T) {
	s := newCoalescer(60)
	s.shouldSend("default", "device-1")
	if !s.shouldSend("default", "device-2") {
		t.Error("different device should not be rate-limited")
	}
}

func TestShouldSend_AfterExpiry(t *testing.T) {
	s := newCoalescer(1) // 1 second for test
	s.shouldSend("default", "device-1")
	time.Sleep(1100 * time.Millisecond)
	if !s.shouldSend("default", "device-1") {
		t.Error("should be allowed after coalesce window expires")
	}
}

// The coalesce key carries the tenant (MESHSAT-1121). Two tenants may register
// the same device id -- an IMEI is unique in the world but a device id here is
// free-form, and a TAK marker or a phone number certainly is not unique. Keyed
// on the device alone, one tenant's beacon would consume the window and silence
// another tenant's, on a different radio under a different licence.
func TestOneTenantCannotConsumeAnothersCoalesceWindow(t *testing.T) {
	s := newCoalescer(60)

	if !s.shouldSend("t_one", "shared-id") {
		t.Fatal("the first tenant was blocked on its first send")
	}
	if !s.shouldSend("t_two", "shared-id") {
		t.Fatal("a second tenant's device with the same id was rate-limited by the " +
			"first tenant's send: one customer can silence another's APRS beacons")
	}
	if s.shouldSend("t_one", "shared-id") {
		t.Error("the first tenant's own repeat was not rate-limited")
	}
}
