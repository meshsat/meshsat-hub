package plans

import "testing"

// The bug this file exists for.
//
// SetLimit used to assign a whole fresh `Limits{Devices: devices}`. That was
// harmless for exactly as long as Limits had one field, and the moment it had
// two, any tier named in HUB_PLAN_*_DEVICES would silently have its TAK
// allowance reset to zero at startup -- so a paying Crew tenant would be told it
// could have no TAK accounts at all, with nothing in the logs to explain it.
//
// Both setters now read the entry and change one field. These tests fail if
// either one goes back to replacing the struct, which is the only way to keep a
// whole-struct assignment from coming back the next time a field is added.
//
// Mutation-checked: restoring `table[plan] = Limits{Devices: devices}` in
// SetLimit makes TestSetLimitKeepsTheTAKAllowance fail with the zero it used to
// produce, and the mirror-image change makes the other one fail.
func TestSetLimitKeepsTheTAKAllowance(t *testing.T) {
	t.Cleanup(reset)

	before := For(Crew)
	if before.TAKUsers == 0 {
		t.Fatalf("crew starts with no TAK allowance (%+v), so this test could not detect the bug", before)
	}

	if !SetLimit(Crew, 50) {
		t.Fatal("SetLimit refused a known plan")
	}
	after := For(Crew)
	if after.Devices != 50 {
		t.Errorf("devices = %d, want the override 50", after.Devices)
	}
	if after.TAKUsers != before.TAKUsers {
		t.Errorf("overriding the DEVICE ceiling changed the TAK ceiling: %d -> %d. "+
			"SetLimit is replacing the struct instead of setting one field",
			before.TAKUsers, after.TAKUsers)
	}
}

func TestSetTAKUserLimitKeepsTheDeviceAllowance(t *testing.T) {
	t.Cleanup(reset)

	before := For(Fleet)
	if before.Devices == 0 {
		t.Fatalf("fleet starts with no device allowance (%+v)", before)
	}

	if !SetTAKUserLimit(Fleet, 7) {
		t.Fatal("SetTAKUserLimit refused a known plan")
	}
	after := For(Fleet)
	if after.TAKUsers != 7 {
		t.Errorf("TAK users = %d, want the override 7", after.TAKUsers)
	}
	if after.Devices != before.Devices {
		t.Errorf("overriding the TAK ceiling changed the DEVICE ceiling: %d -> %d",
			before.Devices, after.Devices)
	}
}

// An unknown plan is refused rather than created, on both setters: a typo in an
// env var must not invent a tier nobody can be moved off.
func TestTAKUserLimitRefusesUnknownPlans(t *testing.T) {
	t.Cleanup(reset)
	for _, name := range []string{"enterprise", "", "crewe", "FREE "} {
		if name == "" || name == "FREE " {
			// Normalise turns these into real plans on purpose: empty means free,
			// and whitespace/case are tolerated from the database.
			continue
		}
		if SetTAKUserLimit(name, 10) {
			t.Errorf("SetTAKUserLimit(%q) was accepted; unknown plans must be refused", name)
		}
	}
}

// Every shipped tier must allow at least one TAK user, or TAK is advertised as a
// feature of every plan and is unusable on one of them.
func TestEveryPlanAllowsAtLeastOneTAKUser(t *testing.T) {
	for _, p := range []string{Free, Crew, Fleet, Custom, Beta} {
		l := For(p)
		if l.TAKUsers != Unlimited && l.TAKUsers < 1 {
			t.Errorf("plan %q allows %d TAK users; TAK is a feature of every plan including free",
				p, l.TAKUsers)
		}
	}
}

// The ceilings must increase with the tier, or a customer paying more gets less.
func TestTAKCeilingsRiseWithTheTier(t *testing.T) {
	free, crew, fleet := For(Free).TAKUsers, For(Crew).TAKUsers, For(Fleet).TAKUsers
	if free >= crew || crew >= fleet {
		t.Errorf("TAK ceilings do not rise: free=%d crew=%d fleet=%d", free, crew, fleet)
	}
}

// AllowsAnotherTAKUser is the gate, and it must behave exactly like its device
// sibling: strictly less than the ceiling, and always true when unlimited.
func TestAllowsAnotherTAKUserAtTheBoundary(t *testing.T) {
	t.Cleanup(reset)
	if !SetTAKUserLimit(Free, 2) {
		t.Fatal("could not set up the boundary")
	}
	for _, tc := range []struct {
		current int
		want    bool
	}{{0, true}, {1, true}, {2, false}, {3, false}} {
		if got := AllowsAnotherTAKUser(Free, tc.current); got != tc.want {
			t.Errorf("AllowsAnotherTAKUser(free, %d) = %v, want %v", tc.current, got, tc.want)
		}
	}
	// An unlimited tier never refuses.
	if !SetTAKUserLimit(Custom, Unlimited) {
		t.Fatal("could not set custom to unlimited")
	}
	if !AllowsAnotherTAKUser(Custom, 10_000) {
		t.Error("an unlimited plan refused another TAK user")
	}
}

// reset puts the package table back to its built-in defaults, so one test's
// override cannot leak into the next.
func reset() { table = cloneDefaults() }
