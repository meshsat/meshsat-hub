package plans

import "testing"

func TestLimitsAndNames(t *testing.T) {
	for _, tc := range []struct {
		plan string
		want int
	}{
		{Free, 4}, {Crew, 24}, {Fleet, 100},
		{Custom, Unlimited}, {Beta, Unlimited},
		{"FREE", 4}, {"  crew  ", 24}, {"", 4},
	} {
		if got := For(tc.plan).Devices; got != tc.want {
			t.Errorf("For(%q).Devices = %d, want %d", tc.plan, got, tc.want)
		}
	}
	// A name nobody recognises is a mistake to notice, not a free unlimited
	// fleet.
	if got := For("enterprise-platinum").Devices; got != 4 {
		t.Errorf("unknown plan got %d devices, want the free limit", got)
	}
}

func TestAllowsAnother(t *testing.T) {
	for _, tc := range []struct {
		plan    string
		current int
		want    bool
	}{
		{Free, 0, true}, {Free, 3, true}, {Free, 4, false}, {Free, 9, false},
		{Crew, 23, true}, {Crew, 24, false},
		{Custom, 1_000_000, true},
		// A tenant already over its cap after a downgrade is refused a new
		// device and nothing else. Everything it has keeps working; that is
		// asserted where the handler is tested.
		{Free, 100, false},
	} {
		if got := AllowsAnother(tc.plan, tc.current); got != tc.want {
			t.Errorf("AllowsAnother(%q, %d) = %v, want %v", tc.plan, tc.current, got, tc.want)
		}
	}
}

func TestSetLimitRefusesUnknownPlans(t *testing.T) {
	t.Cleanup(func() { table = cloneDefaults() })
	if !SetLimit(Crew, 50) || For(Crew).Devices != 50 {
		t.Error("SetLimit should override a known plan")
	}
	// A typo in an env var must not mint a tier nobody can be moved off.
	if SetLimit("crw", 999) {
		t.Error("SetLimit accepted a plan name that does not exist")
	}
	if Known("crw") {
		t.Error("Known() admitted an invented plan")
	}
}
