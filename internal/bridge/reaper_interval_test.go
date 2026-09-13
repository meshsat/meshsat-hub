package bridge

import (
	"testing"
	"time"
)

// MESHSAT-1117. The reaper's check interval used to be half the one global
// timeout. Now a tenant may choose a SHORTER timeout than the platform default,
// and a tick sized for the default would let that choice do nothing for most of
// its range: a tenant on 60 seconds would still only be looked at every 150.
//
// So the interval is half the shortest timeout anyone is allowed to set.
func TestTheReaperTicksForTheShortestTimeoutAnyoneMayChoose(t *testing.T) {
	for _, tc := range []struct {
		name           string
		timeout, floor time.Duration
		want           time.Duration
	}{
		{"the floor is shorter, so it decides", 5 * time.Minute, time.Minute, 30 * time.Second},
		{"the default is shorter, so it decides", time.Minute, 5 * time.Minute, 30 * time.Second},
		{"no floor configured falls back to the default", 5 * time.Minute, 0, 150 * time.Second},
		{"never faster than ten seconds", 12 * time.Second, time.Second, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReaperWithFloor(nil, tc.timeout, tc.floor)
			if r.interval != tc.want {
				t.Errorf("interval = %s, want %s (timeout %s, floor %s)",
					r.interval, tc.want, tc.timeout, tc.floor)
			}
		})
	}
}

// The platform default is still what a bridge with no tenant choice is measured
// against, and the constructor must keep carrying it.
func TestTheReaperKeepsThePlatformDefaultAsItsTimeout(t *testing.T) {
	r := NewReaperWithFloor(nil, 5*time.Minute, time.Minute)
	if r.timeout != 5*time.Minute {
		t.Errorf("timeout = %s, want 5m: this is the value passed to the store as the "+
			"default for tenants that chose nothing", r.timeout)
	}
}
