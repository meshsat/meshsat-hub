package ratelimit

import (
	"testing"
	"time"
)

func TestAllow_FirstSend(t *testing.T) {
	l := NewDeviceLimiter(10, 1.0/60.0, 100, 0, nil)
	if !l.Allow("device-1", false) {
		t.Error("first send should be allowed")
	}
}

func TestAllow_SOSBypass(t *testing.T) {
	l := NewDeviceLimiter(0.1, 0, 0, 0, nil) // nearly empty bucket, zero refill
	// Drain the bucket
	l.mu.Lock()
	b := l.getBucket("device-1")
	b.tokens = 0
	l.mu.Unlock()

	if !l.Allow("device-1", true) {
		t.Error("SOS should always be allowed even with empty bucket")
	}
}

func TestAllow_BucketDrains(t *testing.T) {
	l := NewDeviceLimiter(3, 0, 0, 0, nil) // 3 tokens, no refill, no daily cap

	// First 3 should succeed
	for i := 0; i < 3; i++ {
		if !l.Allow("device-1", false) {
			t.Errorf("send %d should be allowed", i+1)
		}
	}

	// 4th should be throttled
	if l.Allow("device-1", false) {
		t.Error("4th send should be throttled (bucket empty)")
	}
}

func TestAllow_BucketRefills(t *testing.T) {
	l := NewDeviceLimiter(2, 100, 0, 0, nil) // 2 max, 100/s refill (fast for testing)

	// Drain
	l.Allow("device-1", false)
	l.Allow("device-1", false)
	if l.Allow("device-1", false) {
		t.Error("should be throttled after drain")
	}

	// Wait for refill (100/s = 1 token in 10ms)
	time.Sleep(20 * time.Millisecond)

	if !l.Allow("device-1", false) {
		t.Error("should be allowed after refill")
	}
}

func TestAllow_DailyCap(t *testing.T) {
	l := NewDeviceLimiter(100, 100, 5, 0, nil) // generous bucket, 5/day cap

	for i := 0; i < 5; i++ {
		if !l.Allow("device-1", false) {
			t.Errorf("send %d should be allowed (under daily cap)", i+1)
		}
	}

	if l.Allow("device-1", false) {
		t.Error("6th send should be blocked by daily cap")
	}

	// SOS still bypasses
	if !l.Allow("device-1", true) {
		t.Error("SOS should bypass daily cap")
	}
}

func TestAllow_PerDevice(t *testing.T) {
	l := NewDeviceLimiter(2, 0, 0, 0, nil) // 2 tokens each, no refill

	l.Allow("device-1", false)
	l.Allow("device-1", false)

	// device-1 exhausted, device-2 should still work
	if l.Allow("device-1", false) {
		t.Error("device-1 should be throttled")
	}
	if !l.Allow("device-2", false) {
		t.Error("device-2 should not be affected by device-1")
	}
}

func TestOverride(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 0, 0, nil)
	l.Allow("device-1", false) // drain

	if l.Allow("device-1", false) {
		t.Error("should be throttled")
	}

	// Set override
	SetOverride("device-1", 1*time.Hour)
	if !l.Allow("device-1", false) {
		t.Error("should be allowed with override")
	}

	// Clear override
	ClearOverride("device-1")
	if l.Allow("device-1", false) {
		t.Error("should be throttled after override cleared")
	}
}

func TestUsage(t *testing.T) {
	l := NewDeviceLimiter(10, 1.0/60.0, 50, 0, nil)
	l.Allow("device-1", false)
	l.Allow("device-1", false)

	usage := l.Usage("device-1")
	if usage.DeviceID != "device-1" {
		t.Errorf("device: %q", usage.DeviceID)
	}
	if usage.DailySent != 2 {
		t.Errorf("daily sent: %d, want 2", usage.DailySent)
	}
	if usage.DailyCap != 50 {
		t.Errorf("daily cap: %d, want 50", usage.DailyCap)
	}
	if usage.TokensLeft > 9 {
		t.Errorf("tokens should be < 9: %f", usage.TokensLeft)
	}
	if usage.Throttled {
		t.Error("should not be throttled yet")
	}
}

func TestAllUsage(t *testing.T) {
	l := NewDeviceLimiter(10, 1.0/60.0, 0, 0, nil)
	l.Allow("device-1", false)
	l.Allow("device-2", false)

	all := l.AllUsage()
	if len(all) != 2 {
		t.Errorf("expected 2 devices, got %d", len(all))
	}
}

func TestAllow_MonthlyCap(t *testing.T) {
	l := NewDeviceLimiter(100, 100, 0, 5, nil) // no daily cap, 5/month cap

	for i := 0; i < 5; i++ {
		if !l.Allow("device-1", false) {
			t.Errorf("send %d should be allowed (under monthly cap)", i+1)
		}
	}

	if l.Allow("device-1", false) {
		t.Error("6th send should be blocked by monthly cap")
	}

	// SOS bypasses monthly cap
	if !l.Allow("device-1", true) {
		t.Error("SOS should bypass monthly cap")
	}
}

func TestAllow_DailyAndMonthlyCap(t *testing.T) {
	l := NewDeviceLimiter(100, 100, 3, 10, nil) // 3/day, 10/month

	// Hit daily cap
	for i := 0; i < 3; i++ {
		if !l.Allow("device-1", false) {
			t.Errorf("send %d should be allowed", i+1)
		}
	}
	if l.Allow("device-1", false) {
		t.Error("should be blocked by daily cap")
	}
}

func TestUsage_IncludesMonthly(t *testing.T) {
	l := NewDeviceLimiter(10, 1.0/60.0, 50, 500, nil)
	l.Allow("device-1", false)
	l.Allow("device-1", false)

	usage := l.Usage("device-1")
	if usage.MonthlySent != 2 {
		t.Errorf("monthly sent: %d, want 2", usage.MonthlySent)
	}
	if usage.MonthlyCap != 500 {
		t.Errorf("monthly cap: %d, want 500", usage.MonthlyCap)
	}
}
