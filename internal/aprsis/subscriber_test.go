package aprsis

import (
	"testing"
	"time"
)

func TestShouldSend_FirstTime(t *testing.T) {
	s := &Subscriber{
		coalesceSec: 60,
		lastSent:    make(map[string]time.Time),
	}

	if !s.shouldSend("device-1") {
		t.Error("first send should be allowed")
	}
}

func TestShouldSend_RateLimited(t *testing.T) {
	s := &Subscriber{
		coalesceSec: 60,
		lastSent:    make(map[string]time.Time),
	}

	s.shouldSend("device-1") // first send
	if s.shouldSend("device-1") {
		t.Error("second send within coalesce window should be blocked")
	}
}

func TestShouldSend_DifferentDevices(t *testing.T) {
	s := &Subscriber{
		coalesceSec: 60,
		lastSent:    make(map[string]time.Time),
	}

	s.shouldSend("device-1")
	if !s.shouldSend("device-2") {
		t.Error("different device should not be rate-limited")
	}
}

func TestShouldSend_AfterExpiry(t *testing.T) {
	s := &Subscriber{
		coalesceSec: 1, // 1 second for test
		lastSent:    make(map[string]time.Time),
	}

	s.shouldSend("device-1")
	time.Sleep(1100 * time.Millisecond)
	if !s.shouldSend("device-1") {
		t.Error("should be allowed after coalesce window expires")
	}
}
